package main

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("migrations", "migrations").WithPathSelect(shared.NewSelect("*.sql")),
)

type Settings struct {
	DatabaseName string `yaml:"database-name"`
	HotReload    bool   `yaml:"hot-reload"`

	NoMigration bool `yaml:"no-migration"` // Developer only

	// LogLevel controls clickhouse-server log verbosity (trace, debug,
	// information, warning, error, none). Empty = the image default.
	LogLevel string `yaml:"log-level"`

	// Image overrides the default clickhouse-server image. Format "name:tag".
	Image string `yaml:"docker-image"`

	// MigrationSources lets SEVERAL services share this ONE database while each
	// owns its own migrations/ folder. Each source is applied with its own
	// golang-migrate tracking table (schema_migrations_<name>), so the per-source
	// integer version sequences never collide. This service's own ./migrations
	// dir is always applied first with the default table (schema_migrations).
	//
	//   migration-sources:
	//     - name: events       # applies ../events/migrations (default path)
	//     - name: metrics
	//       path: ../metrics/db/migrations
	//
	// Paths are relative to this service's directory (or absolute). A source
	// whose directory is missing is skipped with a warning. Mirrors the postgres
	// agent so the shared-database story is identical across both stores.
	MigrationSources []MigrationSource `yaml:"migration-sources"`
}

// MigrationSource declares one additional service contributing migrations to
// the shared database. See Settings.MigrationSources.
type MigrationSource struct {
	// Name identifies the lineage and names its tracking table
	// (schema_migrations_<name>). Must be a safe SQL identifier ([A-Za-z0-9_]).
	Name string `yaml:"name"`
	// Path is the migrations directory, relative to this service's directory or
	// absolute. Empty defaults to ../<name>/migrations.
	Path string `yaml:"path"`
}

const HotReload = "hot-reload"
const DatabaseName = "database-name"

// clickhouse/clickhouse-server:25.3 is a recent LTS release. The native TCP
// protocol (port 9000) is what the clickhouse-go driver — and golang-migrate's
// clickhouse driver — speak, so that is the endpoint codefly maps. The HTTP
// interface (8123) is not exposed by the single TCP endpoint. Override per
// service via Settings.Image.
var image = &resources.DockerImage{Name: "clickhouse/clickhouse-server", Tag: "25.3"}

// dockerImage returns the configured clickhouse image: the Settings.Image
// override if set, else the default.
func (s *Service) dockerImage() *resources.DockerImage {
	if s.Settings != nil && s.Settings.Image != "" {
		return resources.NewDockerImage(s.Settings.Image)
	}
	return image
}

type Service struct {
	*services.Base

	// Settings
	*Settings

	clickhouseUser     string
	clickhousePassword string
	connection         string

	TcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &agentv0.AgentInformation{
		// Advertise the nix runtime (implemented in nixch.go via
		// RuntimeContextNix) so the CLI's per-service Docker-free gate
		// (flow.resolveDockerFallback → Runner.SupportsNix) lets this service
		// fall back to a nix-provisioned native process when Docker is
		// unreachable. Without it the run hard-stops with "requires Docker"
		// even though the nix path works.
		RuntimeRequirements: []*agentv0.Runtime{
			{Type: agentv0.Runtime_NIX},
		},
		Capabilities: []*agentv0.Capability{
			{Type: agentv0.Capability_BUILDER},
			{Type: agentv0.Capability_RUNTIME},
		},
		Protocols: []*agentv0.Protocol{},
		ConfigurationDetails: []*agentv0.ConfigurationValueDetail{
			{
				Name: "clickhouse", Description: "clickhouse credentials",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "connection", Description: "connection string"},
				}},
		},
		ReadMe: readme,
	}, nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	var err error
	s.clickhouseUser, err = resources.GetConfigurationValue(ctx, conf, "clickhouse", "CLICKHOUSE_USER")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get user")
	}
	s.clickhousePassword, err = resources.GetConfigurationValue(ctx, conf, "clickhouse", "CLICKHOUSE_PASSWORD")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get password")
	}
	return nil
}

// nativeDSN is the clickhouse-go v1 / golang-migrate DSN (tcp:// scheme) used
// by the agent itself for readiness probes and migrations. address is host:port.
func (s *Service) nativeDSN(address string) string {
	return fmt.Sprintf("tcp://%s?username=%s&password=%s&database=%s",
		address, s.clickhouseUser, s.clickhousePassword, s.DatabaseName)
}

// consumerConnection is the URL exported to dependent services. The modern
// clickhouse:// URL form is understood by clickhouse-go v2 and most ClickHouse
// clients/ORMs. address is host:port.
func (s *Service) consumerConnection(address string) string {
	return fmt.Sprintf("clickhouse://%s:%s@%s/%s",
		s.clickhouseUser, s.clickhousePassword, address, s.DatabaseName)
}

func (s *Service) createConnectionString(ctx context.Context, conf *basev0.Configuration, address string) (string, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if err := s.LoadConfiguration(ctx, conf); err != nil {
		return "", s.Wool.Wrapf(err, "cannot get user and password")
	}
	return s.consumerConnection(address), nil
}

func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	connection, err := s.createConnectionString(ctx, conf, instance.Address)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create connection string")
	}

	outputConf := &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: connection, Secret: true},
				},
			},
		},
	}
	return outputConf, nil
}

// sanitizeLogLevel keeps the configured clickhouse log level to a known-safe
// token (it is interpolated into the server config / env).
func sanitizeLogLevel(lvl string) string {
	switch strings.ToLower(strings.TrimSpace(lvl)) {
	case "trace", "debug", "information", "warning", "error", "none":
		return strings.ToLower(strings.TrimSpace(lvl))
	default:
		return ""
	}
}

func main() {
	svc := NewService()
	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: NewRuntime(),
		Builder: NewBuilder(),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
