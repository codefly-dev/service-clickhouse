package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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

type DockerTemplating struct {
	ConnectionStringKeyHolder string
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

	connectionKey := resources.ServiceSecretConfigurationKey(s.Base.Identity, "clickhouse", "connection")
	docker := DockerTemplating{ConnectionStringKeyHolder: fmt.Sprintf("{%s}", connectionKey)}

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

	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters: DeploymentTemplateParameters{
			WithMigration: s.WithMigration(),
			ManagedImage:  s.dockerImage().FullName(),
		},
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.GetNetworkMappings(), s.TcpEndpoint, resources.NewPublicNetworkAccess())
			if err != nil {
				return err
			}
			configuration, err := s.CreateConnectionConfiguration(ctx, req.GetConfiguration(), instance)
			if err != nil {
				return err
			}
			s.Wool.Debug("exporting configuration", wool.Field("conf", resources.MakeConfigurationSummary(configuration)))
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
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
	endpoint.Visibility = resources.VisibilityExternal
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
