package main

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/wool"

	// clickhouse-go v1 registers the "clickhouse" database/sql driver (also used
	// by golang-migrate's clickhouse driver). file:// migration source.
	_ "github.com/ClickHouse/clickhouse-go"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

type Runtime struct {
	services.RuntimeServer
	*Service

	// internal
	runnerEnvironment *dockerrun.DockerEnvironment

	// nixRuntime is set instead of runnerEnvironment when the caller requests
	// RuntimeContextNix — clickhouse then runs natively from a nix-provisioned
	// binary (no Docker), serving the same connection string + database.
	nixRuntime *nixClickhouse

	clickhousePort uint16
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:     s.Settings,
		Requirements: requirements,
		ResolveEndpoints: func(ctx context.Context, endpoints []*basev0.Endpoint) error {
			s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(endpoints)))
			endpoint, err := resources.FindTCPEndpoint(ctx, endpoints)
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find TCP endpoint")
			}
			s.TcpEndpoint = endpoint
			return nil
		},
	})
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)
	s.Runtime.WithContext(req.GetRuntimeContext())

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings
	configuration := req.GetConfiguration()

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}
	if instance == nil {
		return s.Runtime.InitError(w.NewError("network instance is nil"))
	}

	w.Debug("tcp network instance", wool.Field("instance", instance))

	s.Infof("will run on %s", instance.Host)
	s.clickhousePort = 9000

	// Create connection string resources for the network instance
	for _, inst := range net.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}

	// Credentials (clickhouse user/password) are needed by both runtimes and to
	// build the native DSN used for migrations + readiness.
	if err = s.LoadConfiguration(ctx, configuration); err != nil {
		return s.Runtime.InitError(err)
	}

	hostInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.connection = s.nativeDSN(hostInstance.Address)
	w.Debug("native dsn ready")

	// Nix runtime: run clickhouse natively from a nix-provisioned binary.
	if rc := req.GetRuntimeContext(); rc != nil && rc.Kind == resources.RuntimeContextNix {
		w.Debug("using nix runtime for clickhouse", wool.Field("port", instance.Port))
		nixch, errNix := newNixClickhouse(ctx, s.Location, uint16(instance.Port),
			s.clickhouseUser, s.clickhousePassword, s.DatabaseName, sanitizeLogLevel(s.LogLevel), newCHLogWriter(s.Wool))
		if errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		if errNix = nixch.Init(ctx); errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		s.nixRuntime = nixch
		s.Wool.Debug("nix clickhouse init successful")
		if errNix = s.migrateOnInit(ctx); errNix != nil {
			return s.Runtime.InitError(errNix)
		}
		return s.Runtime.InitResponse()
	}

	// Docker
	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, s.dockerImage(), s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithOutput(newCHLogWriter(s.Wool))
	runner.WithPortMapping(ctx, uint16(instance.Port), s.clickhousePort)

	runner.WithEnvironmentVariables(
		ctx,
		resources.Env("CLICKHOUSE_USER", s.clickhouseUser),
		resources.Env("CLICKHOUSE_PASSWORD", s.clickhousePassword),
		resources.Env("CLICKHOUSE_DB", s.DatabaseName),
		// Let the bootstrap user manage databases/tables created by migrations.
		resources.Env("CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT", "1"))

	s.runnerEnvironment = runner

	w.Debug("init for runner environment: will start container")
	if err = s.runnerEnvironment.Init(ctx); err != nil {
		return s.Runtime.InitError(err)
	}

	s.Wool.Debug("init successful")
	if err := s.migrateOnInit(ctx); err != nil {
		return s.Runtime.InitError(err)
	}
	return s.Runtime.InitResponse()
}

// migrateOnInit applies schema migrations DURING Init — after the database is
// up but BEFORE Init returns — so "port reachable" implies "schema migrated",
// closing the readiness race for fast consumers. Start re-applies migrations
// idempotently (migrate.ErrNoChange). Mirrors the postgres agent.
func (s *Runtime) migrateOnInit(ctx context.Context) error {
	if err := s.WaitForReady(ctx); err != nil {
		return err
	}
	if s.Settings.NoMigration {
		return nil
	}
	return s.applyMigration(ctx)
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("waiting for ready")

	// One pool, opened once and reused for every probe (sql.Open is lazy).
	db, err := sql.Open("clickhouse", s.connection)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot open database")
	}
	defer db.Close()
	maxRetry := 30
	var lastErr error
	for range maxRetry {
		if err = db.Ping(); err == nil {
			if _, err = db.Exec("SELECT 1"); err == nil {
				s.Wool.Debug("database ready!")
				return nil
			}
		}
		lastErr = err
		s.Wool.Debug("waiting for clickhouse to be ready", wool.ErrField(err))
		time.Sleep(2 * time.Second)
	}
	tail := ""
	if s.runnerEnvironment != nil {
		tail = s.runnerEnvironment.TailLogs(ctx, 30)
	}
	if tail != "" {
		return s.Wool.NewError("clickhouse not ready after %d retries (last probe: %v); container logs (tail 30):\n%s", maxRetry, lastErr, tail)
	}
	return s.Wool.NewError("clickhouse not ready after %d retries (last probe: %v)", maxRetry, lastErr)
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	if err := s.WaitForReady(ctx); err != nil {
		return s.Runtime.StartError(err)
	}

	if !s.Settings.NoMigration {
		s.Wool.Debug("applying migrations")
		if err := s.applyMigration(ctx); err != nil {
			return s.Runtime.StartError(err)
		}

		if s.Settings.HotReload {
			conf := services.NewWatchConfiguration(requirements)
			if err := s.SetupWatcher(ctx, conf, s.EventHandler); err != nil {
				s.Wool.Warn("error in watcher", wool.ErrField(err))
			}
		}
	}
	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	s.Wool.Debug("nothing to stop: keep environment alive")
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	// Nix runtime: stop the native clickhouse process.
	if s.nixRuntime != nil {
		if err := s.nixRuntime.Stop(ctx); err != nil {
			return s.Runtime.DestroyError(err)
		}
		return s.Runtime.DestroyResponse()
	}

	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, s.dockerImage(), s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	if err = runner.Shutdown(ctx); err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

func (s *Runtime) EventHandler(event code.Change) error {
	if strings.Contains(event.Path, "migrations") {
		if err := s.updateMigration(context.Background(), event.Path); err != nil {
			s.Wool.Warn("cannot apply migration", wool.ErrField(err))
		}
	}
	return nil
}
