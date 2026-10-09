package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The classifier a restricted render applies is core code compiled INTO this
// agent, so the agent's core pin decides what it refuses. Pinned to core v0.5.0,
// this agent classified plain composition values -- a model token limit, an
// issuer URL -- as credentials and refused every staging render that carried
// them (codefly-dev/core#747). These keys are the exact ones that failed.
func TestCompiledClassifierAdmitsPlainCompositionValues(t *testing.T) {
	for _, key := range []string{"max-output-tokens", "AUTHORITY_ISSUER", "max-input-bytes", "audience", "vertex-project"} {
		require.False(t, resources.IsSensitiveKey(key), "%q is a plain value and must not be classified as a credential", key)
	}
	for _, key := range []string{"CODEFLY_INTERNAL_TOKEN", "CLICKHOUSE_PASSWORD", "operator-read-token"} {
		require.True(t, resources.IsSensitiveKey(key), "%q is a credential and must still be classified as one", key)
	}
}

func restrictedRenderBuilder(t *testing.T) *Builder {
	t.Helper()
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{Workspace: "workspace", Module: "tracing", Name: "evidence", Version: "1.2.3"}
	require.NoError(t, builder.HeadlessLoad(ctx, identity))
	builder.Information.Service = resources.ToServiceWithCase(builder.Identity)
	builder.Information.Module = resources.ToModuleWithCase(builder.Identity)
	builder.Settings.DatabaseName = "tracing_evidence"
	builder.TcpEndpoint = &basev0.Endpoint{Name: "tcp", Module: identity.Module, Service: identity.Name, Api: "tcp"}
	return builder
}

const renderImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func renderRequest(destination string, builder *Builder, profile builderv0.KubernetesOutputProfile, configuration *basev0.Configuration, dependencies ...*basev0.Configuration) *builderv0.DeploymentRequest {
	instance := resources.NewNetworkInstance("evidence.platform-obin-tracing.svc.cluster.local", 9000)
	instance.Access = resources.NewPublicNetworkAccess()
	return &builderv0.DeploymentRequest{
		Environment: &basev0.Environment{Name: "staging"},
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				Namespace:   "platform-obin-tracing",
				Destination: destination,
				Profile:     profile,
				// The migration Job's image: a restricted render refuses any
				// image that is not digest-pinned, so the build context has to
				// carry one for the tree to validate at all.
				BuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com", ImageDigest: renderImageDigest},
			},
		}},
		NetworkMappings: []*basev0.NetworkMapping{{
			Endpoint:  builder.TcpEndpoint,
			Instances: []*basev0.NetworkInstance{instance},
		}},
		Configuration:              configuration,
		DependenciesConfigurations: dependencies,
	}
}

func restrictedRenderRequest(destination string, builder *Builder, configuration *basev0.Configuration, dependencies ...*basev0.Configuration) *builderv0.DeploymentRequest {
	req := renderRequest(destination, builder, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1, configuration, dependencies...)
	// What the CLI declares for a restricted render (cli
	// pkg/orchestration/builder_deploy.go promotableConfiguration): a typed
	// Secret reference for every secret value in the request, i.e. this
	// service's own secret keys -- and nothing for a value the agent exports.
	references := map[string]*builderv0.KubernetesSecretKeyReference{}
	for _, key := range []string{"CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"} {
		carrier := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "clickhouse", key)
		references[carrier] = &builderv0.KubernetesSecretKeyReference{Name: "platform-obin-tracing-evidence", Key: carrier}
	}
	req.GetDeployment().GetKubernetes().SecretReferences = references
	return req
}

// The composition-root groups every service receives, with the two plain
// values that blocked core#747, plus this service's own secret configuration
// as a restricted render carries it: keys declared, values absent.
func rootPlainValues() *basev0.Configuration {
	return &basev0.Configuration{
		Origin: "_workspace_origin",
		Infos: []*basev0.ConfigurationInformation{
			{Name: "model-installation", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "max-output-tokens", Value: "2048"}}},
			{Name: "runtime-execution", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "AUTHORITY_ISSUER", Value: "https://auth.staging.example"}}},
		},
	}
}

func ownSecretsWithoutValues() *basev0.Configuration {
	return &basev0.Configuration{
		Origin: "tracing/evidence",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "clickhouse",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "CLICKHOUSE_USER", Secret: true},
				{Key: "CLICKHOUSE_PASSWORD", Secret: true},
			},
		}},
	}
}

func readRenderedTree(t *testing.T, dir string) string {
	t.Helper()
	var tree strings.Builder
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		tree.WriteString("\n### " + path + "\n")
		tree.Write(content)
		return nil
	})
	require.NoError(t, err)
	return tree.String()
}

// A composition-root group reaches every service, so a plain integer in the
// root .env reaches THIS agent's restricted render. The render must admit it,
// and it must then succeed without a single credential in the tree: the
// connection consumers read is exported as a template over the user and
// password, delivered by the Secret reference declared for its carrier.
func TestRestrictedRenderAdmitsPlainRootValuesAndExportsATemplatedConnection(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	destination := t.TempDir()
	response, err := builder.Deploy(ctx, restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues()))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	output := response.GetDeployment().GetKubernetes()
	require.True(t, output.GetValidation().GetRestricted())
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, output.GetValidation().GetStaticValidation())

	connection, err := resources.GetConfigurationInformation(ctx, response.GetConfiguration(), "clickhouse")
	require.NoError(t, err)
	require.Len(t, connection.GetConfigurationValues(), 1)
	value := connection.GetConfigurationValues()[0]
	require.Equal(t, "connection", value.GetKey())
	require.True(t, value.GetSecret())
	require.Empty(t, value.GetValue(), "a restricted render never assembles the connection")
	require.NoError(t, resources.ValidateTemplatedConfigurationValue(value))
	require.NotContains(t, readRenderedTree(t, destination), "CLICKHOUSE__CONNECTION",
		"the exported connection is not a workload input; consumers assemble it from the template")
	// What the environment's secret store will assemble from the primitives:
	// the same bytes the ephemeral render hands out (see connection_template_test.go).
	assembled, err := resources.EvaluateConfigurationValueTemplate(value.GetTemplate(), func(configuration, key string) (string, bool) {
		return map[string]string{"CLICKHOUSE_USER": "evidence", "CLICKHOUSE_PASSWORD": "s3cr3t"}[key], true
	})
	require.NoError(t, err)
	require.Equal(t, "clickhouse://evidence:s3cr3t@evidence.platform-obin-tracing.svc.cluster.local:9000/tracing_evidence", assembled)

	tree := readRenderedTree(t, destination)
	require.NotContains(t, tree, "kind: Secret")
	require.NotContains(t, tree, "\ndata:")
	require.NotContains(t, tree, "\nstringData:")
	require.NotContains(t, tree, "2048")
	require.NotContains(t, tree, "auth.staging.example")
	require.Contains(t, tree, "kind: StatefulSet")
	require.Contains(t, tree, "kind: Job", "migrations are enabled, so the migration Job is rendered")
	require.Contains(t, tree, image.FullName(), "the digest-pinned server image is unchanged")
}

// The ephemeral profile still assembles the connection from the values it
// holds, percent-encoding them as URL userinfo so the bytes agree with the
// template, and refuses a missing credential by name instead of exporting
// "clickhouse://:@host/db".
func TestEphemeralRenderAssemblesAnEscapedConnectionAndRefusesEmptyCredentials(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	own := func(user, password string) *basev0.Configuration {
		return &basev0.Configuration{
			Origin: "tracing/evidence",
			Infos: []*basev0.ConfigurationInformation{{
				Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "CLICKHOUSE_USER", Value: user, Secret: true},
					{Key: "CLICKHOUSE_PASSWORD", Value: password, Secret: true},
				},
			}},
		}
	}
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1

	response, err := builder.Deploy(ctx, renderRequest(t.TempDir(), builder, ephemeral, own("evidence", "p@ss:w/rd?&+ 1")))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	connection, err := resources.GetConfigurationValue(ctx, response.GetConfiguration(), "clickhouse", "connection")
	require.NoError(t, err)
	require.Equal(t, "clickhouse://evidence:p%40ss%3Aw%2Frd%3F%26%2B%201@evidence.platform-obin-tracing.svc.cluster.local:9000/tracing_evidence", connection)

	// A passwordless user is a mode the Nix runtime supports (<no_password/>),
	// and the URL states it honestly rather than refusing it.
	response, err = builder.Deploy(ctx, renderRequest(t.TempDir(), builder, ephemeral, own("evidence", "")))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	connection, err = resources.GetConfigurationValue(ctx, response.GetConfiguration(), "clickhouse", "connection")
	require.NoError(t, err)
	require.Equal(t, "clickhouse://evidence:@evidence.platform-obin-tracing.svc.cluster.local:9000/tracing_evidence", connection)

	for name, configuration := range map[string]*basev0.Configuration{
		"empty user": own("", "secret"),
		"no values":  ownSecretsWithoutValues(),
	} {
		t.Run(name, func(t *testing.T) {
			response, err := builder.Deploy(ctx, renderRequest(t.TempDir(), builder, ephemeral, configuration))
			require.NoError(t, err)
			require.Equal(t, builderv0.DeploymentStatus_ERROR, response.GetState().GetState())
			require.Contains(t, response.GetState().GetMessage(), "clickhouse credentials: CLICKHOUSE_USER is empty")
		})
	}
}
