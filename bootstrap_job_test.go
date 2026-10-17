package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The bootstrap Job must carry the database URL its entrypoint migrates
// against. It used to carry only the clickhouse user and password, and the
// entrypoint — handed a connection variable nothing ever set — exited with
// golang-migrate's own `failed to parse scheme from database URL: URL cannot
// be empty`, so the schema was never created.
//
// The contract has two halves rendered by two different calls: Build bakes the
// variable NAMES into the image's entrypoint, Deploy delivers their VALUES to
// the Job. This drives both halves from one builder, under BOTH output
// profiles, and holds them against each other: every variable the entrypoint
// expands must be one the Job can actually see, and the address must be the
// in-cluster instance the deploy handed this endpoint.
func TestBootstrapJobCarriesEveryVariableItsEntrypointReads(t *testing.T) {
	for _, profile := range []builderv0.KubernetesOutputProfile{
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
	} {
		t.Run(profile.String(), func(t *testing.T) {
			ctx := context.Background()
			useSuccessfulKubectl(t)
			builder := loadedBuilder(t)
			// A restricted manifest only validates with a digest-pinned
			// image, as a real Build sets.
			builder.Base.SetDockerImage(image)

			expanded, address := entrypointVariables(ctx, t, builder)

			destination := t.TempDir()
			requireRendered(ctx, t, builder, profile, destination)

			job, err := os.ReadFile(filepath.Join(destination, "base", "job.yaml"))
			require.NoError(t, err)
			manifest := string(job)

			// One source for the address: the handed in-cluster instance, the
			// same one the Service publishes. It is carried as a value because
			// it is not a credential.
			require.Contains(t, manifest, "- name: "+address+"\n              value: \""+inClusterAddress+"\"",
				"the Job must carry the in-cluster address the deploy handed this endpoint:\n%s", manifest)

			visible := jobEnvironment(t, destination, "test", manifest)
			for variable := range expanded {
				require.Truef(t, visible[variable],
					"the entrypoint expands %s but the Job cannot see it; it sees %v", variable, keys(visible))
			}
		})
	}
}

// inClusterAddress is the instance a deploy hands this endpoint: the address
// the Service publishes and every consumer, including the bootstrap Job,
// dials.
const inClusterAddress = "clickhouse.codefly-test.svc.cluster.local:9000"

// deployRender renders the manifest tree under one output profile, the way the
// host delivers this service's credentials under it: as values an inline-secret
// render writes into the Secret its containers load, or as references a
// restricted render resolves from the environment's own store -- it may not
// even RECEIVE a secret value. references overrides what the host declares.
func deployRender(
	ctx context.Context,
	t *testing.T,
	builder *Builder,
	profile builderv0.KubernetesOutputProfile,
	destination string,
	references map[string]*builderv0.KubernetesSecretKeyReference,
) (*builderv0.DeploymentResponse, error) {
	t.Helper()
	container := resources.NewNetworkInstance("clickhouse.codefly-test.svc.cluster.local", 9000)
	container.Access = resources.NewContainerNetworkAccess()
	require.Equal(t, inClusterAddress, container.GetAddress())
	public := resources.NewNetworkInstance("clickhouse.example.com", 9000)
	public.Access = resources.NewPublicNetworkAccess()

	kubernetes := &builderv0.KubernetesDeployment{
		Namespace:   "codefly-test",
		Destination: destination,
		Profile:     profile,
	}
	configuration := clickhouseCredentials(builder.Unique())
	if profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1 {
		configuration = nil
		kubernetes.SecretReferences = references
		if kubernetes.SecretReferences == nil {
			// The host declares them under this service's own clickhouse
			// carriers: the names the bootstrap Job's entrypoint reads, which
			// the StatefulSet aliases under the names the engine reads.
			kubernetes.SecretReferences = map[string]*builderv0.KubernetesSecretKeyReference{
				builder.migrationUserKey():     {Name: "clickhouse-credentials", Key: "user"},
				builder.migrationPasswordKey(): {Name: "clickhouse-credentials", Key: "password"},
			}
		}
	}
	return builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment:   &basev0.Environment{Name: "test"},
		Configuration: configuration,
		NetworkMappings: []*basev0.NetworkMapping{{
			Endpoint:  builder.TcpEndpoint,
			Instances: []*basev0.NetworkInstance{public, container},
		}},
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{Kubernetes: kubernetes}},
	})
}

// requireRendered deploys and insists the render succeeded.
func requireRendered(
	ctx context.Context,
	t *testing.T,
	builder *Builder,
	profile builderv0.KubernetesOutputProfile,
	destination string,
) {
	t.Helper()
	deploy, err := deployRender(ctx, t, builder, profile, destination, nil)
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, deploy.GetState().GetState(), deploy.GetState().GetMessage())
}

// entrypointVariables builds the migration image recipe and reads back off the
// rendered entrypoint every codefly variable it expands — assumed nowhere,
// since drift between the two halves is the bug under test. It returns that
// set and the address key.
func entrypointVariables(ctx context.Context, t *testing.T, builder *Builder) (map[string]bool, string) {
	t.Helper()
	recipe := t.TempDir()
	build, err := builder.Build(ctx, dockerBuildRequest(recipe))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, build.GetState().GetState(), build.GetState().GetMessage())

	content, err := os.ReadFile(filepath.Join(recipe, "builder", "Dockerfile"))
	require.NoError(t, err)
	dockerfile := string(content)

	address, user, password := builder.migrationAddressKey(), builder.migrationUserKey(), builder.migrationPasswordKey()
	// clickhouse-go v1 — the driver behind the migrate CLI's clickhouse://
	// scheme — reads the credentials and database from the QUERY and ignores
	// URL userinfo, so the consumer form this service exports
	// (clickhouse://user:pass@host/db) would authenticate as `default`.
	require.Contains(t, dockerfile, "-database \"clickhouse://$"+address+"?username=$"+user+"&password=$"+password+"&",
		"the entrypoint must compose the migrate URL from the address and the credentials, in the query form the driver reads")

	expanded := map[string]bool{}
	for _, match := range regexp.MustCompile(`\$(CODEFLY__[A-Z0-9_]+)`).FindAllStringSubmatch(dockerfile, -1) {
		expanded[match[1]] = true
	}
	require.Equal(t, map[string]bool{address: true, user: true, password: true}, expanded)
	return expanded, address
}

// jobEnvironment is every variable name the Job's container can see: the env
// entries it declares, plus the keys of the Secret it loads with envFrom.
func jobEnvironment(t *testing.T, destination, environment, manifest string) map[string]bool {
	t.Helper()
	visible := map[string]bool{}
	for _, line := range strings.Split(manifest, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "- name: "); ok {
			visible[name] = true
		}
	}
	if !strings.Contains(manifest, "envFrom:") {
		return visible
	}
	secret, err := os.ReadFile(filepath.Join(destination, "overlays", environment, "secret.yaml"))
	require.NoError(t, err, "the Job loads a Secret with envFrom, so the render must carry one")
	for _, line := range strings.Split(string(secret), "\n") {
		key, _, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.HasPrefix(key, "CODEFLY__") {
			visible[key] = true
		}
	}
	return visible
}

func clickhouseCredentials(origin string) *basev0.Configuration {
	return &basev0.Configuration{
		Origin: origin,
		Infos: []*basev0.ConfigurationInformation{{
			Name: "clickhouse",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "CLICKHOUSE_USER", Value: "user", Secret: true},
				{Key: "CLICKHOUSE_PASSWORD", Value: "password", Secret: true},
			},
		}},
	}
}

func keys(set map[string]bool) []string {
	var names []string
	for name := range set {
		names = append(names, name)
	}
	return names
}
