package main

import (
	"context"
	"embed"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
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

	// DatabaseName is the database the server creates at first boot
	// (CLICKHOUSE_DB) and the migration Job applies ./migrations to.
	DatabaseName string

	// Host and Port are the in-cluster address consumers are handed (the
	// public network instance): the Service publishes Port and targets the
	// container's 9000, the migration Job dials Host:Port, and the exported
	// connection names the same address. One source, so the three cannot
	// disagree. Port 0 renders as 9000 (a template rendered without a
	// mapping); Deploy refuses it.
	Host string
	Port uint32

	// SecretReferences are the typed Secret references a restricted render
	// delivers the credentials by, to the server and the migration Job alike,
	// keyed by the environment variable they read (CLICKHOUSE_USER,
	// CLICKHOUSE_PASSWORD). Empty under the ephemeral profile, whose generated
	// Secret carries the same names with their values.
	SecretReferences map[string]*builderv0.KubernetesSecretKeyReference

	// MigrationsDigest is a sha256 over ./migrations, set on the Job's pod
	// template so a changed migration under an unchanged image ref is a
	// changed Job (and, through JobName, a new one).
	MigrationsDigest string

	// JobName is the migration Job's name: the service name plus a digest of
	// the Job's rendered pod template, so a changed Job is a new object (a
	// Job's pod template is immutable) and an unchanged one re-applies as a
	// no-op. Required whenever the Job renders.
	JobName string
}

// dockerImage returns the configured clickhouse image: the Settings.Image
// override if set, else the default. An override this agent cannot parse is
// handed through verbatim as the image name, so the container runtime names
// the problem; Deploy refuses it by name first (configuredImage).
func (s *Service) dockerImage() *resources.DockerImage {
	configured, err := s.configuredImage()
	if err != nil {
		return &resources.DockerImage{Name: s.Settings.Image}
	}
	return configured
}

// configuredImage is dockerImage with the parse error kept.
func (s *Service) configuredImage() (*resources.DockerImage, error) {
	if s.Settings != nil && s.Settings.Image != "" {
		return parseImageOverride(s.Settings.Image)
	}
	return image, nil
}

var (
	imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageTag    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// A repository path component, as the distribution reference grammar
	// states it: lower-case alphanumerics joined by ".", "_", "__" or "-"s.
	imagePathComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	// A registry host, optionally with a numeric port.
	imageRegistry = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?$`)
)

// parseImageOverride reads a docker-image override: "name", "name:tag",
// "name@sha256:<hex>" or "name:tag@sha256:<hex>", where name may carry a
// registry with a port ("registry:5000/clickhouse"). core's NewDockerImage
// splits on every ":", so it returns nil for a digest-pinned reference and
// reads a registry port as a tag; a restricted render needs the digest form,
// so this agent parses the reference itself, and refuses what a container
// runtime would refuse rather than render it.
func parseImageOverride(reference string) (*resources.DockerImage, error) {
	if reference == "" {
		return nil, fmt.Errorf("clickhouse docker-image override is empty")
	}
	if strings.IndexFunc(reference, unicode.IsSpace) >= 0 {
		return nil, fmt.Errorf("clickhouse docker-image %q contains whitespace", reference)
	}
	parsed := &resources.DockerImage{}
	remainder := reference
	if name, digest, pinned := strings.Cut(remainder, "@"); pinned {
		if !imageDigest.MatchString(digest) {
			return nil, fmt.Errorf("clickhouse docker-image %q: the digest must be sha256:<64 lower-case hex>", reference)
		}
		parsed.Digest = digest
		remainder = name
	}
	name := remainder
	if separator := strings.LastIndex(remainder, ":"); separator > strings.LastIndex(remainder, "/") {
		name, parsed.Tag = remainder[:separator], remainder[separator+1:]
		if !imageTag.MatchString(parsed.Tag) {
			return nil, fmt.Errorf("clickhouse docker-image %q: the tag must match %s", reference, imageTag)
		}
	}
	components := strings.Split(name, "/")
	for i, component := range components {
		// The first of several components is a registry when it looks like a
		// host (a ".", a ":" port, or localhost), as docker reads it.
		registry := i == 0 && len(components) > 1 &&
			(strings.ContainsAny(component, ".:") || component == "localhost")
		if registry && !imageRegistry.MatchString(component) {
			return nil, fmt.Errorf("clickhouse docker-image %q: %q is not a registry host[:port]", reference, component)
		}
		if !registry && !imagePathComponent.MatchString(component) {
			return nil, fmt.Errorf("clickhouse docker-image %q: repository component %q must be lower-case alphanumerics joined by '.', '_' or '-'", reference, component)
		}
	}
	parsed.Name = name
	if parsed.Tag == "" && parsed.Digest == "" {
		parsed.Tag = "latest"
	}
	return parsed, nil
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

// LoadConfiguration keeps the credentials on the Service for the runtime,
// which owns one server and builds its own DSN from them. Deploy never calls
// it: one Builder serves concurrent Deploys, so a render reads the credentials
// into locals (credentials) and never through shared fields.
func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	user, password, err := s.credentials(ctx, conf)
	if err != nil {
		return err
	}
	s.clickhouseUser, s.clickhousePassword = user, password
	return nil
}

// credentials reads CLICKHOUSE_USER and CLICKHOUSE_PASSWORD from the service's
// own "clickhouse" configuration, writing nothing.
func (s *Service) credentials(ctx context.Context, conf *basev0.Configuration) (user, password string, err error) {
	user, err = resources.GetConfigurationValue(ctx, conf, "clickhouse", "CLICKHOUSE_USER")
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get user")
	}
	password, err = resources.GetConfigurationValue(ctx, conf, "clickhouse", "CLICKHOUSE_PASSWORD")
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get password")
	}
	return user, password, nil
}

// requiredCredentials is credentials with the empty user refused. A missing
// key reads as "" with no error, and "clickhouse://:@host/db" is a credential
// that is silently absent: the server would come up with the image's default
// user and every consumer would be told to use another. The password is not
// refused, because a passwordless user is a mode the Nix runtime deliberately
// supports (<no_password/>, nixch.go) and the URL "user:@host" states it
// honestly.
func (s *Service) requiredCredentials(ctx context.Context, conf *basev0.Configuration) (user, password string, err error) {
	user, password, err = s.credentials(ctx, conf)
	if err != nil {
		return "", "", s.Wool.Wrapf(err, "cannot get user and password")
	}
	if user == "" {
		return "", "", s.Wool.NewError("clickhouse credentials: CLICKHOUSE_USER is empty")
	}
	return user, password, nil
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
// address is the host:port the connection names.
func (s *Service) promotableConnectionConfiguration(instance *basev0.NetworkInstance, address string) *basev0.Configuration {
	return &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Secret: true, Template: clickhouseConnectionTemplate(address, s.DatabaseName)},
				},
			},
		},
	}
}

// CreateConnectionConfiguration is the assembled connection for one network
// instance (the runtime exports one per instance). It reads the credentials
// into locals: it writes no Service field.
func (s *Service) CreateConnectionConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	user, password, err := s.requiredCredentials(ctx, conf)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create connection string")
	}
	return s.assembledConnectionConfiguration(instance, instance.Address, user, password), nil
}

// assembledConnectionConfiguration exports the connection assembled from the
// credentials it is handed, naming address (host:port).
func (s *Service) assembledConnectionConfiguration(instance *basev0.NetworkInstance, address, user, password string) *basev0.Configuration {
	return &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "connection", Value: clickhouseConnectionString(address, s.DatabaseName, user, password), Secret: true},
				},
			},
		},
	}
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
