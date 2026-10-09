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
	runnersbase "github.com/codefly-dev/core/runners/base"
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

// clickhouse/clickhouse-server:26.3 is the current LTS release. The native TCP
// protocol (port 9000) is what the clickhouse-go driver — and golang-migrate's
// clickhouse driver — speak, so that is the endpoint codefly maps. The HTTP
// interface (8123) is not exposed by the single TCP endpoint. Override per
// service via Settings.Image.
var image = &resources.DockerImage{
	Name:   "clickhouse/clickhouse-server",
	Tag:    "26.3",
	Digest: "sha256:85c434814ac8905e5648027ce926f74ab067edd6aadbccb6c0c165cd3571ea49",
}

type DeploymentTemplateParameters struct {
	WithMigration bool
	ManagedImage  string
}

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

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Nix:    true,
			Docker: true,
		},
		Config: []*agentv0.ConfigurationValueDetail{
			{
				Name: "clickhouse", Description: "clickhouse credentials",
				Fields: []*agentv0.ConfigurationValueInformation{
					{Name: "connection", Description: "connection string"},
				}},
		},
		ReadMe: readme,
	}.Build(), nil
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

// clickhouseConnectionLiterals are the parts of the consumer URL around the
// credentials: "clickhouse://" and "@<address>/<database>". The assembled
// string and the template are built from the same two literals, so a consumer
// is handed the same bytes whether this agent held the password (local, the
// ephemeral render) or only declared where it lives (the restricted render).
func clickhouseConnectionLiterals(address, database string) (prefix, suffix string) {
	return "clickhouse://", "@" + address + "/" + database
}

// clickhouseConnectionString is the URL exported to dependent services. The
// modern clickhouse:// URL form is understood by clickhouse-go v2 and most
// ClickHouse clients/ORMs. User and password are percent-encoded as URL
// userinfo with core's URL_USERINFO escape -- the one the template below
// states -- so a password carrying "@", "/" or "?" cannot move the host the
// URL points at. address is host:port.
func clickhouseConnectionString(address, database, user, password string) string {
	prefix, suffix := clickhouseConnectionLiterals(address, database)
	return prefix + escapeUserinfo(user) + ":" + escapeUserinfo(password) + suffix
}

// escapeUserinfo is core's URL_USERINFO escape, the one the connection template
// states. EscapeConfigurationValue errs only on an escape it does not know,
// and this one is a constant it knows, so the error cannot occur.
func escapeUserinfo(value string) string {
	escaped, err := resources.EscapeConfigurationValue(basev0.ConfigurationValueEscape_CONFIGURATION_VALUE_ESCAPE_URL_USERINFO, value)
	if err != nil {
		panic("clickhouse connection: " + err.Error())
	}
	return escaped
}

// clickhouseConnectionTemplate is clickhouseConnectionString with the user and
// password left as references to this service's own "clickhouse" secret
// configuration. A restricted render never holds those values: the environment's
// secret store assembles the URL where the primitives are, and nobody stores the
// assembled form.
func clickhouseConnectionTemplate(address, database string) *basev0.ConfigurationValueTemplate {
	prefix, suffix := clickhouseConnectionLiterals(address, database)
	reference := func(key string) *basev0.ConfigurationValueTemplateSegment {
		return &basev0.ConfigurationValueTemplateSegment{Content: &basev0.ConfigurationValueTemplateSegment_Reference{
			Reference: &basev0.ConfigurationValueReference{
				Configuration: "clickhouse",
				Key:           key,
				Escape:        basev0.ConfigurationValueEscape_CONFIGURATION_VALUE_ESCAPE_URL_USERINFO,
			},
		}}
	}
	literal := func(text string) *basev0.ConfigurationValueTemplateSegment {
		return &basev0.ConfigurationValueTemplateSegment{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: text}}
	}
	return &basev0.ConfigurationValueTemplate{Segments: []*basev0.ConfigurationValueTemplateSegment{
		literal(prefix), reference("CLICKHOUSE_USER"), literal(":"), reference("CLICKHOUSE_PASSWORD"), literal(suffix),
	}}
}

// promotableConnectionConfiguration is the connection a restricted render
// exports: the same "clickhouse" / "connection" value consumers read today,
// carried as a template over the user and password instead of assembled from
// them. Core delivers a templated secret by the reference the environment
// declares for its carrier, so no credential enters the rendered tree.
func (s *Service) promotableConnectionConfiguration(instance *basev0.NetworkInstance) *basev0.Configuration {
	return &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Secret: true, Template: clickhouseConnectionTemplate(instance.Address, s.DatabaseName)},
				},
			},
		},
	}
}

func (s *Service) createConnectionString(ctx context.Context, conf *basev0.Configuration, address string) (string, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if err := s.LoadConfiguration(ctx, conf); err != nil {
		return "", s.Wool.Wrapf(err, "cannot get user and password")
	}
	// A missing key reads as "" with no error, and "clickhouse://:@host/db" is
	// a credential that is silently absent: the server would come up with the
	// image's default user and every consumer would be told to use another.
	// The user is refused when empty; the password is not, because a
	// passwordless user is a mode the Nix runtime deliberately supports
	// (<no_password/>, nixch.go) and the URL "user:@host" states it honestly.
	if s.clickhouseUser == "" {
		return "", s.Wool.NewError("clickhouse credentials: CLICKHOUSE_USER is empty")
	}
	return clickhouseConnectionString(address, s.DatabaseName, s.clickhouseUser, s.clickhousePassword), nil
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
