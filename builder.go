package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/upgrade"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"
)

type Builder struct {
	services.BuilderServer
	*Service
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
		ResolveEndpoints: func(ctx context.Context, endpoints []*v0.Endpoint) error {
			endpoint, err := resources.FindTCPEndpoint(ctx, endpoints)
			if err != nil {
				return err
			}
			s.TcpEndpoint = endpoint
			s.Wool.Debug("endpoint", wool.Field("tcp", endpoint))
			return nil
		},
	})
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()
	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SyncResponse()
}

// Audit scans the clickhouse image for known CVEs via trivy.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.AuditContainer(ctx, req, s.dockerImage().FullName())
}

func (s *Builder) SBOM(ctx context.Context, _ *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SBOMContainer(ctx, s.dockerImage().FullName())
}

// Upgrade reports a tag bump from the current clickhouse image.
func (s *Builder) Upgrade(ctx context.Context, req *builderv0.UpgradeRequest) (*builderv0.UpgradeResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	res, err := upgrade.Docker(ctx, image.FullName(), upgrade.Options{
		IncludeMajor: req.IncludeMajor,
		DryRun:       req.DryRun,
	})
	if err != nil {
		return s.Builder.UpgradeError(err)
	}
	return s.Builder.UpgradeResponse(res.Changes, res.LockfileDiff)
}

// DockerTemplating is the data the builder templates render with. The
// migration image takes nothing from the build: migrate.sh assembles its
// connection at run time from the environment the Job is given, so no carrier
// name or credential is baked into the image.
type DockerTemplating struct{}

func (s *Builder) WithMigration() bool {
	return !s.Settings.NoMigration
}

// errMigrationSourcesOnKubernetes is returned by Build and Deploy when the
// service declares migration-sources: the local runtime applies each source
// with its own tracking table, but the migration image carries ./migrations
// only, so a Kubernetes deployment would silently apply part of the schema.
func errMigrationSourcesOnKubernetes() error {
	return fmt.Errorf("clickhouse migration-sources are applied by the local runtime only; the migration Job applies ./migrations: remove migration-sources (or move those migrations into ./migrations) to build or deploy this service for Kubernetes")
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	if !s.WithMigration() {
		s.Wool.Debug("build: no migration")
		return s.Builder.BuildResponse()
	}
	if len(s.Settings.MigrationSources) > 0 {
		return s.Builder.BuildError(errMigrationSourcesOnKubernetes())
	}

	ctx = s.Wool.Inject(ctx)

	dockerRequest, err := s.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "can only do docker build request")
	}

	img := s.DockerImage(dockerRequest)

	if !dockerhelpers.IsValidDockerImageName(img.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker image name: %s", img.Name))
	}

	docker := DockerTemplating{}

	if output := req.GetOutputDirectory(); output != "" {
		return s.buildRecipe(ctx, output, docker, img)
	}

	return s.buildImage(ctx, docker, img)
}

// buildImage runs the legacy in-agent docker build. It is retained for callers
// that do not supply an output_directory (a CLI that has not adopted the
// CLI-owned build).
func (s *Builder) buildImage(ctx context.Context, docker DockerTemplating, img *resources.DockerImage) (*builderv0.BuildResponse, error) {
	s.Wool.Debug("building migration docker image")

	err := shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "builder/Dockerfile",
		Destination: img,
		Output:      s.Wool,
	})
	if err != nil {
		return s.Builder.BuildError(err)
	}
	_, err = builder.Build(ctx)
	if err != nil {
		return s.Builder.BuildError(err)
	}

	s.Builder.WithDockerImages(img)
	return s.Builder.BuildResponse()
}

// buildRecipe renders the migration image recipe — the Dockerfile plus the
// migrations build context — into the CLI-owned output directory and returns a
// DockerBuildPlan. The CLI runs docker buildx from the recipe and pushes a
// multi-arch manifest list, so the image is a durable artifact a consumer can
// rebuild without the agent toolchain.
func (s *Builder) buildRecipe(ctx context.Context, output string, docker DockerTemplating, img *resources.DockerImage) (*builderv0.BuildResponse, error) {
	s.Wool.Debug("rendering migration image recipe", wool.DirField(output))

	// The recipe tree must be exactly what this build renders: BuildDockerBuildPlan
	// inventories whatever is on disk and the CLI verifies the same tree, so a file
	// left by a prior build into a reused output_directory would be digested into
	// the plan and baked into the image without ever failing verification. Clear
	// the trees this build owns before writing them.
	for _, sub := range []string{"builder", "migrations"} {
		if err := os.RemoveAll(filepath.Join(output, sub)); err != nil {
			return s.Builder.BuildError(err)
		}
	}

	err := s.Templates(ctx, docker, services.WithBuilder(builderFS).WithDestination("%s", filepath.Join(output, "builder")))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = copyTree(ctx, s.Local("migrations"), filepath.Join(output, "migrations"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	recipe := &builderv0.DockerBuildRecipe{
		Name:       "migration",
		Dockerfile: "builder/Dockerfile",
		Context:    ".",
		Image:      img.FullName(),
		Platforms:  []string{"linux/amd64", "linux/arm64"},
	}
	plan, err := services.BuildDockerBuildPlan(output, []*builderv0.DockerBuildRecipe{recipe})
	if err != nil {
		return s.Builder.BuildError(err)
	}

	s.Builder.WithBuildPlan(plan)
	return s.Builder.BuildResponse()
}

func copyTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return shared.CopyFile(ctx, p, target)
	})
}

// restrictedOutput reports whether the deployment selects the restricted,
// portable output contract: no secret value may enter the rendered tree. The
// profile was judged by core before Prepare ran, so an error here cannot occur
// for a request that reached this agent; it is read as restricted all the same,
// because the other reading would hand a restricted render the secrets it
// exists to refuse.
func restrictedOutput(profile builderv0.KubernetesOutputProfile) bool {
	parsed, err := services.ParseOutputProfile(profile)
	if err != nil {
		return true
	}
	return parsed.Restricted()
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	// A pointer: Prepare fills the parameters the templates read (image,
	// address, Secret references, Job name).
	parameters := &DeploymentTemplateParameters{
		WithMigration: s.WithMigration(),
		DatabaseName:  s.DatabaseName,
	}
	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters:           parameters,
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			configuration, err := s.prepareDeployment(ctx, deployment, parameters)
			if err != nil {
				return err
			}
			if parameters.WithMigration {
				if parameters.JobName, err = s.immutableMigrationJobName(deployment, parameters); err != nil {
					return err
				}
			}
			s.Wool.Debug("exporting configuration", wool.Field("conf", resources.MakeConfigurationSummary(configuration)))
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
}

// prepareDeployment fills the template parameters both profiles render with and
// returns the connection configuration to export. The image reads its
// credentials as CLICKHOUSE_USER / CLICKHOUSE_PASSWORD / CLICKHOUSE_DB; the
// restricted profile delivers the first two as typed Secret references, the
// ephemeral one as values in its generated Secret under the same names, so one
// template path serves both.
func (s *Builder) prepareDeployment(
	ctx context.Context,
	deployment *services.KustomizeDeploymentContext,
	parameters *DeploymentTemplateParameters,
) (*v0.Configuration, error) {
	restricted := restrictedOutput(deployment.Profile)
	if parameters.WithMigration && len(s.Settings.MigrationSources) > 0 {
		return nil, errMigrationSourcesOnKubernetes()
	}
	if !clickHouseDatabaseName.MatchString(parameters.DatabaseName) {
		// The image creates this database at first boot and the Job and the
		// connection name it; an empty one silently means `default`, and
		// anything outside the identifier charset would need quoting in YAML,
		// in the URL and in ClickHouse alike.
		return nil, fmt.Errorf("clickhouse database-name %q must be a ClickHouse identifier matching %s", parameters.DatabaseName, clickHouseDatabaseName)
	}
	managed, err := s.configuredImage()
	if err != nil {
		return nil, err
	}
	if restricted && managed.Digest == "" {
		// core refuses a digest-less image in a restricted tree too, but only
		// after rendering and without saying which setting put it there.
		return nil, fmt.Errorf("clickhouse docker-image %q has no digest: a restricted render requires a digest-pinned image (name:tag@sha256:<64 hex>)", s.Settings.Image)
	}
	parameters.ManagedImage = managed.FullName()

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, deployment.Request.GetNetworkMappings(), s.TcpEndpoint, resources.NewPublicNetworkAccess())
	if err != nil {
		return nil, err
	}
	host, port, err := deploymentAddress(instance)
	if err != nil {
		return nil, err
	}
	// One address for all three readers: the Service publishes Port (targeting
	// 9000), the Job dials Host:Port, and the connection names it.
	parameters.Host, parameters.Port = host, port
	address := net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))
	if parameters.WithMigration {
		if parameters.MigrationsDigest, err = migrationsDigest(s.Local("migrations")); err != nil {
			return nil, err
		}
	}

	if restricted {
		// The restricted render never loads the credentials. The server and
		// the migration Job read them by typed Secret reference. Consumers
		// receive the connection as a template over them: the CLI declares
		// Secret references only for values already in the request (these
		// two), never for a value the agent exports, and each consumer's
		// ExternalSecret assembles the connection from the template.
		references, err := s.selectPromotableSecretReferences(deployment.Kubernetes.GetSecretReferences())
		if err != nil {
			return nil, err
		}
		parameters.SecretReferences = references
		return s.promotableConnectionConfiguration(instance, address), nil
	}

	// Locals, never Service fields: one Builder serves concurrent Deploys.
	user, password, err := s.requiredCredentials(ctx, deployment.Request.GetConfiguration())
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot create connection string")
	}
	// The raw workload values stay in the ephemeral profile's generated Secret,
	// under the names the image and migrate.sh read; consumers receive only
	// the connection.
	deployment.AddSecrets(
		resources.Env("CLICKHOUSE_USER", user),
		resources.Env("CLICKHOUSE_PASSWORD", password),
	)
	return s.assembledConnectionConfiguration(instance, address, user, password), nil
}

// clickHouseDatabaseName is the charset a deployed database-name is held to:
// an unquoted ClickHouse identifier.
var clickHouseDatabaseName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// clickhouseHTTPPort is the container's HTTP interface, which the Service also
// publishes as itself.
const clickhouseHTTPPort = 8123

// deploymentAddress is the one host and port a deployment derives everything
// from: the instance's port, and its hostname (else the host of its address).
// An address that names another host or port is refused rather than guessed
// between, and so are port 0 (no allocation) and 8123 (the Service's HTTP
// port: two Service ports cannot share it).
func deploymentAddress(instance *v0.NetworkInstance) (string, uint32, error) {
	port := instance.GetPort()
	if port == 0 || port > 65535 {
		return "", 0, fmt.Errorf("clickhouse network instance %q has no usable port (%d)", instance.GetAddress(), port)
	}
	if port == clickhouseHTTPPort {
		return "", 0, fmt.Errorf("clickhouse network instance port %d collides with the Service's HTTP port %d", port, clickhouseHTTPPort)
	}
	host := instance.GetHostname()
	if address := instance.GetAddress(); address != "" {
		addressHost, addressPort, err := net.SplitHostPort(address)
		if err != nil {
			return "", 0, fmt.Errorf("clickhouse network instance address %q: %w", address, err)
		}
		if addressPort != strconv.FormatUint(uint64(port), 10) || (host != "" && addressHost != host) {
			return "", 0, fmt.Errorf("clickhouse network instance is inconsistent: address %q but hostname %q and port %d", address, host, port)
		}
		host = addressHost
	}
	if host == "" {
		return "", 0, fmt.Errorf("clickhouse network instance has no host")
	}
	return host, port, nil
}

// migrationsDigest is a sha256 over the migrations directory: every regular
// file's slash-separated relative path and content, in path order. A missing
// directory digests as empty. It goes on the Job's pod template, so new
// migrations shipped under an unchanged image ref still make a new Job.
func migrationsDigest(dir string) (string, error) {
	digest := sha256.New()
	var paths []string
	err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("digest migrations: %w", err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		content, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("digest migrations: %w", err)
		}
		relative, err := filepath.Rel(dir, p)
		if err != nil {
			return "", fmt.Errorf("digest migrations: %w", err)
		}
		fmt.Fprintf(digest, "%s\x00%d\x00", filepath.ToSlash(relative), len(content))
		digest.Write(content)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// clickhouseCredentialEnvironment are the credentials the image (and the
// migration Job) read, each authored as the same key of this service's own
// "clickhouse" secret configuration.
var clickhouseCredentialEnvironment = []string{"CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"}

// selectPromotableSecretReferences resolves, for a restricted render, the typed
// Secret reference each credential arrives by. The references are declared for
// the carriers of this service's own "clickhouse" secret configuration; the
// workload reads them under the image's environment names. A missing one is a
// misconfigured service, and saying so is the point: rendering without it would
// boot the server as the image's passwordless default user.
func (s *Builder) selectPromotableSecretReferences(
	configured map[string]*builderv0.KubernetesSecretKeyReference,
) (map[string]*builderv0.KubernetesSecretKeyReference, error) {
	selected := make(map[string]*builderv0.KubernetesSecretKeyReference, len(clickhouseCredentialEnvironment))
	for _, environmentVariable := range clickhouseCredentialEnvironment {
		configurationKey := resources.ServiceSecretConfigurationKeyFromUnique(s.Unique(), "clickhouse", environmentVariable)
		reference := configured[configurationKey]
		if reference == nil || reference.GetName() == "" || reference.GetKey() == "" {
			return nil, fmt.Errorf("clickhouse deployment requires a typed Kubernetes Secret reference for %s", configurationKey)
		}
		if reference.GetOptional() {
			return nil, fmt.Errorf("%s Kubernetes Secret reference must not be optional", configurationKey)
		}
		selected[environmentVariable] = reference
	}
	return selected, nil
}

const migrationJobTemplatePath = "templates/deployment/kustomize/base/job.yaml.tmpl"

// immutableMigrationJobName names the migration Job after its own pod
// template. A Job's pod template is immutable, so a fixed name makes a GitOps
// re-apply of a changed Job (new image, new address, new references) fail; a
// name that follows the content makes the changed Job a new object and an
// unchanged one a no-op. The Job template is rendered here exactly as core
// will render it, and only spec.template is digested, so the name itself is
// not part of what it names.
func (s *Builder) immutableMigrationJobName(
	deployment *services.KustomizeDeploymentContext,
	parameters *DeploymentTemplateParameters,
) (string, error) {
	service := shared.ToDNSCase(s.Identity.Name)
	if service == "" {
		return "", fmt.Errorf("migration Job: service name is required")
	}
	source, err := fs.ReadFile(deploymentFS, migrationJobTemplatePath)
	if err != nil {
		return "", fmt.Errorf("read migration Job template: %w", err)
	}
	jobTemplate, err := template.New(migrationJobTemplatePath).Parse(string(source))
	if err != nil {
		return "", fmt.Errorf("parse migration Job template: %w", err)
	}
	renderContext := &services.DeploymentWrapper{
		DeploymentBase: &services.DeploymentBase{
			Information: s.Information,
			Namespace:   deployment.Kubernetes.GetNamespace(),
			Image:       s.DockerImage(deployment.Kubernetes.GetBuildContext()),
			Profile:     deployment.Profile,
			Restricted:  restrictedOutput(deployment.Profile),
		},
		Deployment: services.DeploymentParameters{Parameters: parameters},
	}
	var rendered bytes.Buffer
	if err = jobTemplate.Execute(&rendered, renderContext); err != nil {
		return "", fmt.Errorf("render migration Job template: %w", err)
	}
	var job struct {
		Spec struct {
			Template yaml.Node `yaml:"template"`
		} `yaml:"spec"`
	}
	if err = yaml.Unmarshal(rendered.Bytes(), &job); err != nil {
		return "", fmt.Errorf("parse rendered migration Job: %w", err)
	}
	if job.Spec.Template.Kind == 0 {
		return "", fmt.Errorf("rendered migration Job is missing spec.template")
	}
	podTemplate, err := yaml.Marshal(&job.Spec.Template)
	if err != nil {
		return "", fmt.Errorf("encode migration Job pod template: %w", err)
	}
	digest := sha256.Sum256(podTemplate)

	const suffixLength = 12
	const maxServiceLength = 63 - 1 - suffixLength
	if len(service) > maxServiceLength {
		service = strings.TrimRight(service[:maxServiceLength], "-")
	}
	return service + "-" + hex.EncodeToString(digest[:])[:suffixLength], nil
}

type create struct {
	DatabaseName string
	TableName    string
}

var clickHouseResourceName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// clickHouseTableName turns a valid Codefly resource name into a safe,
// unquoted ClickHouse identifier. Codefly names commonly contain hyphens,
// which ClickHouse parses as subtraction when emitted directly into DDL.
func clickHouseTableName(name string) (string, error) {
	if !clickHouseResourceName.MatchString(name) {
		return "", fmt.Errorf("service name %q cannot be used as a ClickHouse table name", name)
	}
	return strings.ReplaceAll(name, "-", "_"), nil
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	s.Settings.HotReload = true
	if s.Settings.DatabaseName == "" {
		s.Settings.DatabaseName = s.Base.Identity.Module
	}

	tableName, err := clickHouseTableName(s.Builder.Service.Name)
	if err != nil {
		return s.Builder.CreateError(err)
	}
	c := create{DatabaseName: s.Settings.DatabaseName, TableName: tableName}

	err = s.Templates(ctx, c, services.WithFactory(factoryFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return s.Builder.CreateErrorf(err, "cannot create endpoints")
	}

	s.Wool.Debug("created endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load tcp api")
	}
	endpoint := s.Base.BaseEndpoint(standards.TCP)
	// PRIVATE, not the retired `external`. `external` is no longer a
	// visibility at all -- reach is visibility, addressing is exposure, and
	// where it lives is location -- and on a store it was a false claim
	// either way: the render allocates this service its own in-cluster
	// address, so nothing about it is external. Every composed store already
	// declares `visibility: private` in its own manifest.
	endpoint.Visibility = resources.VisibilityPrivate
	s.TcpEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToTCPAPI(tcp))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create tcp api")
	}
	s.Endpoints = []*v0.Endpoint{s.TcpEndpoint}
	return nil
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	_, err := asker.RunSequence(nil)
	return err
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
