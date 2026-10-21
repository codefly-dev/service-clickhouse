package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

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
	"gopkg.in/yaml.v3"
)

type Builder struct {
	services.BuilderServer
	*Service
}

// BuildCapabilities answers the CLI's check before a build that names a Buildx
// builder: this agent executes no docker build of its own — its Build hands
// the CLI a recipe or no image at all — so a selected builder is always
// honoured, by the CLI, which runs every recipe on the builder it selected.
// Without this answer core refuses the whole render of any environment that
// mirrors a registry, because such an environment builds on a dedicated
// builder and core verifies every agent before dispatching to it.
func (s *Builder) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{BuildxSelection: true}, nil
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

// DockerTemplating names the variables the migration image's entrypoint
// expands at run time, plus the database it migrates. The entrypoint COMPOSES
// the migrate URL rather than being handed one: the address comes from the
// single variable the bootstrap Job carries from the render, the credentials
// from the Secret the Job already reads. Two reasons it is built there and not
// rendered whole: a restricted render may carry no secret value (and no
// literal value under a credential-like key), and the connection this service
// exports is the consumer form -- clickhouse://user:pass@host/db -- whose
// userinfo clickhouse-go v1, the driver behind the migrate CLI, does not read
// at all.
type DockerTemplating struct {
	AddressKey   string
	UserKey      string
	PasswordKey  string
	DatabaseName string
}

func (s *Builder) WithMigration() bool {
	return !s.Settings.NoMigration
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	if !s.WithMigration() {
		s.Wool.Debug("build: no migration")
		return s.Builder.BuildResponse()
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

	docker := DockerTemplating{
		AddressKey:   s.migrationAddressKey(),
		UserKey:      s.migrationUserKey(),
		PasswordKey:  s.migrationPasswordKey(),
		DatabaseName: s.Settings.DatabaseName,
	}

	if output := req.GetOutputDirectory(); output != "" {
		return s.buildRecipe(ctx, output, docker, img)
	}
	// The legacy in-agent build runs docker itself and cannot honour a
	// selected Buildx builder; BuildCapabilities promises the selection is
	// honoured, so a caller that selects one is held to recipe execution.
	if selected := req.GetBuildContext().GetDockerBuildContext().GetBuildxBuilder(); selected != "" {
		return s.Builder.BuildError(fmt.Errorf("a selected Buildx builder %q requires recipe execution (output_directory); the in-agent build cannot honour it", selected))
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

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	var restrictedConfiguration *v0.Configuration
	parameters := &DeploymentTemplateParameters{
		WithMigration: s.WithMigration(),
		ManagedImage:  s.dockerImage().FullName(),
		Server:        s.serverEnvironment(),
	}
	response, err := s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters:           parameters,
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			// The Service publishes the port core allocated to the endpoint —
			// the one every in-cluster consumer dials — and targets the native
			// port. Same mechanism as postgres: the mapping the CLI hands this
			// Deploy carries our own endpoint.
			container, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.GetNetworkMappings(), s.TcpEndpoint, resources.NewContainerNetworkAccess())
			if err != nil {
				return err
			}
			if container.GetPort() == 0 || container.GetPort() > 65535 {
				return fmt.Errorf("endpoint %s: the delivered container-access instance has no usable port (%d)", s.TcpEndpoint.Name, container.GetPort())
			}
			parameters.ServicePort = container.GetPort()
			// The bootstrap Job migrates against the SAME handed instance the
			// Service publishes -- host and port from one lookup, so the Job
			// can never dial a port the Service does not publish.
			if container.GetAddress() == "" {
				return fmt.Errorf("endpoint %s: the delivered container-access instance has no address for the bootstrap Job to migrate against", s.TcpEndpoint.Name)
			}
			parameters.MigrationAddressKey = s.migrationAddressKey()
			parameters.MigrationAddress = container.GetAddress()
			// Every input the Job template reads is settled by now, so the
			// Job can be named after what it will run.
			jobName, err := s.bootstrapJobName(deployment, parameters)
			if err != nil {
				return err
			}
			parameters.BootstrapJobName = jobName
			instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.GetNetworkMappings(), s.TcpEndpoint, resources.NewPublicNetworkAccess())
			if err != nil {
				return err
			}
			if restrictedOutput(deployment.Profile) {
				// A restricted render carries no secret value: the server reads
				// its credentials from the Secret references the host supplies
				// (see the templates), and consumers receive a value-free
				// reference to the connection the host resolves.
				if err := s.referenceServerCredentials(deployment, parameters); err != nil {
					return err
				}
				restrictedConfiguration = s.restrictedConnectionConfiguration(instance)
				return nil
			}
			configuration, err := s.CreateConnectionConfiguration(ctx, req.GetConfiguration(), instance)
			if err != nil {
				return err
			}
			// The bootstrap Job reads the credentials under their codefly
			// carrier names -- the names a restricted render receives as
			// secret references. The Secret this profile renders carried only
			// the exported connection, which is the CONSUMER form: userinfo,
			// which clickhouse-go v1 (the driver behind the migrate CLI)
			// ignores, so the Job authenticated as `default` against the
			// `default` database wherever it ran at all. Deliver the same two
			// carriers here and both profiles feed ONE entrypoint.
			if s.WithMigration() {
				deployment.AddSecrets(
					resources.Env(s.migrationUserKey(), s.clickhouseUser),
					resources.Env(s.migrationPasswordKey(), s.clickhousePassword),
				)
			}
			// And the engine itself reads them under the image's own names,
			// which no codefly carrier can be: the carriers name a module and
			// a service. Delivered in the same Secret the container loads, so
			// the StatefulSet needs no reference in this profile.
			deployment.AddSecrets(
				resources.Env(clickhouseUserVariable, s.clickhouseUser),
				resources.Env(clickhousePasswordVariable, s.clickhousePassword),
			)
			s.Wool.Debug("exporting configuration", wool.Field("conf", resources.MakeConfigurationSummary(configuration)))
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
	if err != nil ||
		response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS ||
		restrictedConfiguration == nil {
		return response, err
	}
	response.Configuration = restrictedConfiguration
	return response, nil
}

const bootstrapJobTemplatePath = "templates/deployment/kustomize/base/job.yaml.tmpl"

// bootstrapJobName names the migration Job after its content: the service
// name plus the first 12 hex characters of a SHA-256 over the Job's rendered
// pod template. It is the service-postgres bootstrap Job pattern, kept the
// same so a reconciler sees one lifecycle for every migration Job.
//
// Why the content and not the service name: a Job's pod template is
// immutable, so a Job that keeps its name can never carry a new migration
// image. When the render pins that image by digest -- always under a
// restricted profile, which refuses any other image; under local apply only
// when the build captured one -- and the image is built from `migrations/`, a
// changed migration set names a new Job, which runs. So does a changed
// address or credential reference. An unchanged render names the same Job,
// which is left alone. A local apply of a tag-only image under an unchanged
// version keeps the name even when migrations changed, as the postgres
// agent's Job does: bump the version, or render digest-pinned, to rerun them.
// The Job also carries no TTL: it
// stays once complete, so the manifest that declares it stays in sync. A
// fixed name with `ttlSecondsAfterFinished: 0` did neither: Kubernetes
// deleted the Job on completion and a reconciler reported it missing forever.
func (s *Builder) bootstrapJobName(
	deployment *services.KustomizeDeploymentContext,
	parameters *DeploymentTemplateParameters,
) (string, error) {
	service := shared.ToDNSCase(s.Identity.Name)
	if service == "" {
		return "", fmt.Errorf("bootstrap Job: service name is required")
	}
	source, err := fs.ReadFile(deploymentFS, bootstrapJobTemplatePath)
	if err != nil {
		return "", fmt.Errorf("bootstrap Job: read template: %w", err)
	}
	jobTemplate, err := template.New(bootstrapJobTemplatePath).Parse(string(source))
	if err != nil {
		return "", fmt.Errorf("bootstrap Job: parse template: %w", err)
	}
	// The same inputs DeployKustomize renders the template with.
	renderContext := &services.DeploymentWrapper{
		DeploymentBase: &services.DeploymentBase{
			Information: s.Information,
			Namespace:   deployment.Kubernetes.GetNamespace(),
			Image:       s.DockerImage(deployment.Kubernetes.GetBuildContext()),
			Profile:     deployment.Profile,
			Restricted:  restrictedOutput(deployment.Profile),
		},
		Deployment:       services.DeploymentParameters{Parameters: parameters},
		SecretReferences: deployment.Kubernetes.GetSecretReferences(),
	}
	var rendered bytes.Buffer
	if err = jobTemplate.Execute(&rendered, renderContext); err != nil {
		return "", fmt.Errorf("bootstrap Job: render template: %w", err)
	}
	var job struct {
		Spec struct {
			Template yaml.Node `yaml:"template"`
		} `yaml:"spec"`
	}
	if err = yaml.Unmarshal(rendered.Bytes(), &job); err != nil {
		return "", fmt.Errorf("bootstrap Job: parse rendered manifest: %w", err)
	}
	if job.Spec.Template.Kind == 0 {
		return "", fmt.Errorf("bootstrap Job: rendered manifest has no spec.template")
	}
	podTemplate, err := yaml.Marshal(&job.Spec.Template)
	if err != nil {
		return "", fmt.Errorf("bootstrap Job: encode pod template: %w", err)
	}
	digest := sha256.Sum256(podTemplate)

	const suffixLength = 12
	const maxServiceLength = 63 - 1 - suffixLength
	if len(service) > maxServiceLength {
		service = strings.TrimRight(service[:maxServiceLength], "-")
	}
	return service + "-" + hex.EncodeToString(digest[:])[:suffixLength], nil
}

// referenceServerCredentials points the clickhouse-server container at the
// credentials in a render that may carry no secret value: the host's declared
// references for this service's own clickhouse carriers, which the StatefulSet
// aliases under the names the image reads. Those are the same two references
// the bootstrap Job's entrypoint reads by carrier, so one declaration serves
// both containers.
//
// A render that declares neither is refused BY NAME. It is not a render with
// weaker credentials: the engine would come up on its built-in `default` user
// with no password, every consumer would be handed a connection no server
// accepts, and the first sign of it is an authentication failure somewhere
// else entirely.
func (s *Builder) referenceServerCredentials(deployment *services.KustomizeDeploymentContext, parameters *DeploymentTemplateParameters) error {
	references := deployment.Kubernetes.GetSecretReferences()
	user, password := references[s.migrationUserKey()], references[s.migrationPasswordKey()]
	if user == nil || password == nil {
		return fmt.Errorf("restricted rendering has no secret reference for %q and %q: the clickhouse server cannot be given the credentials this service is configured with, and would come up on its built-in `default` user",
			s.migrationUserKey(), s.migrationPasswordKey())
	}
	// A host that ALSO declares a reference under the image's own name already
	// delivers it through the passthrough the templates render, so aliasing it
	// again would render the variable twice and leave a kubelet to resolve the
	// duplicate by taking the last -- a silent choice between two references.
	if references[clickhouseUserVariable] == nil {
		parameters.Server.User = user
	}
	if references[clickhousePasswordVariable] == nil {
		parameters.Server.Password = password
	}
	return nil
}

// restrictedOutput reports whether the deployment selects the restricted,
// portable output contract: no secret value may leave the agent, so the
// connection is advertised as a reference the host resolves.
func restrictedOutput(profile builderv0.KubernetesOutputProfile) bool {
	parsed, err := services.ParseOutputProfile(profile)
	if err != nil {
		return true
	}
	return parsed.Restricted()
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
