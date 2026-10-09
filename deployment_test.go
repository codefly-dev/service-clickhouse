package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDeploymentTemplatesWithMigration(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, templateParameters(true, image.FullName()))
	assertMigrationResource(t, dir, true)
}

func TestDeploymentTemplatesWithoutMigration(t *testing.T) {
	dir := agenttesting.AssertKustomizeTemplates(t, deploymentFS, templateParameters(false, image.FullName()))
	assertMigrationResource(t, dir, false)
}

func assertMigrationResource(t *testing.T, dir string, expected bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Contains(string(content), "- job.yaml"); got != expected {
		t.Fatalf("migration resource present = %t, want %t:\n%s", got, expected, content)
	}
}

// renderedEnv is one container environment entry as the manifest serializes it.
type renderedEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom *struct {
		SecretKeyRef *struct {
			Name     string `yaml:"name"`
			Key      string `yaml:"key"`
			Optional *bool  `yaml:"optional"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

type renderedWorkload struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		TTLSecondsAfterFinished *int `yaml:"ttlSecondsAfterFinished"`
		Template                struct {
			Spec struct {
				Containers []struct {
					Name    string        `yaml:"name"`
					Image   string        `yaml:"image"`
					Env     []renderedEnv `yaml:"env"`
					EnvFrom []struct {
						SecretRef struct {
							Name string `yaml:"name"`
						} `yaml:"secretRef"`
					} `yaml:"envFrom"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func readWorkload(t *testing.T, destination, file string) renderedWorkload {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(destination, "base", file))
	require.NoError(t, err)
	var workload renderedWorkload
	require.NoError(t, yaml.Unmarshal(content, &workload), string(content))
	require.Len(t, workload.Spec.Template.Spec.Containers, 1, string(content))
	return workload
}

func (w renderedWorkload) env() map[string]renderedEnv {
	envs := map[string]renderedEnv{}
	for _, env := range w.Spec.Template.Spec.Containers[0].Env {
		envs[env.Name] = env
	}
	return envs
}

type renderedService struct {
	Spec struct {
		ClusterIP string `yaml:"clusterIP"`
		Ports     []struct {
			Name       string `yaml:"name"`
			Port       int    `yaml:"port"`
			TargetPort int    `yaml:"targetPort"`
		} `yaml:"ports"`
	} `yaml:"spec"`
}

func readService(t *testing.T, destination string) renderedService {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(destination, "base", "service.yaml"))
	require.NoError(t, err)
	var service renderedService
	require.NoError(t, yaml.Unmarshal(content, &service), string(content))
	return service
}

func (s renderedService) native(t *testing.T) (port, target int) {
	t.Helper()
	for _, p := range s.Spec.Ports {
		if p.Name == "native" {
			return p.Port, p.TargetPort
		}
	}
	t.Fatalf("no native port in %+v", s.Spec.Ports)
	return 0, 0
}

// withInstancePort re-points the request's public network instance at the
// port the CLI would have allocated.
func withInstancePort(req *builderv0.DeploymentRequest, port uint32) *builderv0.DeploymentRequest {
	instance := resources.NewNetworkInstance("evidence.platform-obin-tracing.svc.cluster.local", uint16(port))
	instance.Access = resources.NewPublicNetworkAccess()
	req.NetworkMappings[0].Instances = []*basev0.NetworkInstance{instance}
	return req
}

func requireDeployed(t *testing.T, response *builderv0.DeploymentResponse, err error) {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
}

func requireRefused(t *testing.T, response *builderv0.DeploymentResponse, err error, message string) {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_ERROR, response.GetState().GetState())
	require.Contains(t, response.GetState().GetMessage(), message)
}

func ownSecrets(user, password string) *basev0.Configuration {
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

var migrationJobName = regexp.MustCompile(`^evidence-[0-9a-f]{12}$`)

// The restricted render delivers the credentials to the server and to the
// migration Job as typed Secret references under the names the image and
// migrate.sh read, carries no secret value, publishes the port consumers were
// handed, and names the Job after its own pod template.
func TestPromotableDeploymentUsesTypedSecretReferencesWithoutValues(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	render := func(digest string) (string, string) {
		destination := t.TempDir()
		req := restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues())
		req.GetDeployment().GetKubernetes().GetBuildContext().ImageDigest = digest
		response, err := builder.Deploy(ctx, req)
		requireDeployed(t, response, err)
		require.True(t, response.GetDeployment().GetKubernetes().GetValidation().GetRestricted())
		return destination, readWorkload(t, destination, "job.yaml").Metadata.Name
	}
	destination, jobName := render(renderImageDigest)

	for _, key := range []string{"CLICKHOUSE_DB", "CLICKHOUSE_HOST", "CLICKHOUSE_PORT", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT"} {
		require.False(t, resources.IsSensitiveKey(key), "%s is rendered as a literal and must not read as a credential", key)
	}
	for _, file := range []string{"stateful-set.yaml", "job.yaml"} {
		workload := readWorkload(t, destination, file)
		require.Empty(t, workload.Spec.Template.Spec.Containers[0].EnvFrom, "%s: a restricted render imports no generated Secret", file)
		env := workload.env()
		require.Equal(t, "tracing_evidence", env["CLICKHOUSE_DB"].Value, file)
		for _, name := range []string{"CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"} {
			carrier := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "clickhouse", name)
			reference := env[name].ValueFrom
			require.NotNil(t, reference, "%s: %s", file, name)
			require.NotNil(t, reference.SecretKeyRef, "%s: %s", file, name)
			require.Equal(t, "platform-obin-tracing-evidence", reference.SecretKeyRef.Name)
			require.Equal(t, carrier, reference.SecretKeyRef.Key)
			require.NotNil(t, reference.SecretKeyRef.Optional)
			require.False(t, *reference.SecretKeyRef.Optional)
			require.Empty(t, env[name].Value)
		}
	}
	statefulSet := readWorkload(t, destination, "stateful-set.yaml").env()
	require.Equal(t, "1", statefulSet["CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT"].Value)

	job := readWorkload(t, destination, "job.yaml")
	require.Equal(t, "evidence.platform-obin-tracing.svc.cluster.local", job.env()["CLICKHOUSE_HOST"].Value)
	require.Equal(t, "9000", job.env()["CLICKHOUSE_PORT"].Value)
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished)
	require.Equal(t, 3600, *job.Spec.TTLSecondsAfterFinished, "the finished Job is kept long enough to read")
	require.Regexp(t, migrationJobName, jobName)

	port, target := readService(t, destination).native(t)
	require.Equal(t, 9000, port)
	require.Equal(t, 9000, target)
	require.Equal(t, "None", readService(t, destination).Spec.ClusterIP, "nothing to translate, so the Service stays headless")

	tree := readRenderedTree(t, destination)
	require.NotContains(t, tree, "kind: Secret")
	require.NotContains(t, tree, "secret-evidence")
	require.NotContains(t, tree, "s3cr3t")

	_, again := render(renderImageDigest)
	require.Equal(t, jobName, again, "an unchanged Job re-renders under the same name")
	_, changed := render("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.Regexp(t, migrationJobName, changed)
	require.NotEqual(t, jobName, changed, "a Job with a new image is a new Job")
}

// Each credential's reference is required on its own, and an optional one is
// refused: either would boot the server as the image's passwordless default
// user while consumers are told to use another. The two need not share a
// Secret.
func TestPromotableDeploymentRejectsMissingOrOptionalRequiredSecretReferences(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	for _, name := range []string{"CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"} {
		carrier := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "clickhouse", name)
		t.Run(name+"/missing", func(t *testing.T) {
			destination := t.TempDir()
			req := restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues())
			delete(req.GetDeployment().GetKubernetes().SecretReferences, carrier)
			response, err := builder.Deploy(ctx, req)
			requireRefused(t, response, err, "clickhouse deployment requires a typed Kubernetes Secret reference for "+carrier)
			entries, err := os.ReadDir(destination)
			require.NoError(t, err)
			require.Empty(t, entries, "a refused render writes nothing")
		})
		t.Run(name+"/optional", func(t *testing.T) {
			req := restrictedRenderRequest(t.TempDir(), builder, ownSecretsWithoutValues(), rootPlainValues())
			req.GetDeployment().GetKubernetes().SecretReferences[carrier].Optional = true
			response, err := builder.Deploy(ctx, req)
			requireRefused(t, response, err, carrier+" Kubernetes Secret reference must not be optional")
		})
	}
	t.Run("separate Secrets", func(t *testing.T) {
		req := restrictedRenderRequest(t.TempDir(), builder, ownSecretsWithoutValues(), rootPlainValues())
		carrier := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "clickhouse", "CLICKHOUSE_PASSWORD")
		req.GetDeployment().GetKubernetes().SecretReferences[carrier].Name = "another-secret"
		response, err := builder.Deploy(ctx, req)
		requireDeployed(t, response, err)
	})
}

// The ephemeral profile keeps the raw credentials in its generated Secret,
// under the names the image and migrate.sh read, and both workloads import it.
func TestEphemeralDeploymentRetainsRawCredentialsInSecret(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	destination := t.TempDir()
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	response, err := builder.Deploy(ctx, renderRequest(destination, builder, ephemeral, ownSecrets("evidence", "s3cr3t")))
	requireDeployed(t, response, err)

	content, err := os.ReadFile(filepath.Join(destination, "overlays", "staging", "secret.yaml"))
	require.NoError(t, err)
	var secret struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal(content, &secret), string(content))
	require.Equal(t, "secret-evidence", secret.Metadata.Name)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("evidence")), secret.Data["CLICKHOUSE_USER"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("s3cr3t")), secret.Data["CLICKHOUSE_PASSWORD"])

	for _, file := range []string{"stateful-set.yaml", "job.yaml"} {
		workload := readWorkload(t, destination, file)
		envFrom := workload.Spec.Template.Spec.Containers[0].EnvFrom
		require.Len(t, envFrom, 1, file)
		require.Equal(t, "secret-evidence", envFrom[0].SecretRef.Name, file)
		env := workload.env()
		require.Equal(t, "tracing_evidence", env["CLICKHOUSE_DB"].Value, file)
		require.NotContains(t, env, "CLICKHOUSE_USER", "%s reads the credentials from the Secret, not a literal", file)
		require.NotContains(t, env, "CLICKHOUSE_PASSWORD", file)
	}
	require.Regexp(t, migrationJobName, readWorkload(t, destination, "job.yaml").Metadata.Name)
}

// The CLI allocates the instance port consumers dial; the Service publishes it
// and translates to the container's 9000, which a headless Service cannot do.
func TestDeploymentPublishesTheAllocatedInstancePort(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	for name, req := range map[string]func(string) *builderv0.DeploymentRequest{
		"restricted": func(destination string) *builderv0.DeploymentRequest {
			return restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues())
		},
		"ephemeral": func(destination string) *builderv0.DeploymentRequest {
			return renderRequest(destination, builder, ephemeral, ownSecrets("evidence", "s3cr3t"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			destination := t.TempDir()
			response, err := builder.Deploy(ctx, withInstancePort(req(destination), 31234))
			requireDeployed(t, response, err)
			service := readService(t, destination)
			port, target := service.native(t)
			require.Equal(t, 31234, port)
			require.Equal(t, 9000, target)
			require.Empty(t, service.Spec.ClusterIP, "a translated port needs a ClusterIP")
			job := readWorkload(t, destination, "job.yaml").env()
			require.Equal(t, "31234", job["CLICKHOUSE_PORT"].Value)
			require.Equal(t, "evidence.platform-obin-tracing.svc.cluster.local", job["CLICKHOUSE_HOST"].Value)
			// The connection names the same address.
			value := response.GetConfiguration().GetInfos()[0].GetConfigurationValues()[0]
			if template := value.GetTemplate(); template != nil {
				assembled, err := resources.EvaluateConfigurationValueTemplate(template, func(_, key string) (string, bool) { return "x", true })
				require.NoError(t, err)
				require.Contains(t, assembled, "@evidence.platform-obin-tracing.svc.cluster.local:31234/")
			} else {
				require.Contains(t, value.GetValue(), "@evidence.platform-obin-tracing.svc.cluster.local:31234/")
			}
		})
	}
}

// migration-sources are applied by the local runtime with a tracking table
// each; the migration image carries ./migrations only. Rendering anyway would
// apply part of the schema and report success, so Build and Deploy refuse.
func TestDeploymentRefusesMigrationSourcesOnKubernetes(t *testing.T) {
	ctx := context.Background()
	const refusal = "migration-sources are applied by the local runtime only; the migration Job applies ./migrations"

	builder := restrictedRenderBuilder(t)
	builder.Settings.MigrationSources = []MigrationSource{{Name: "events"}}
	destination := t.TempDir()
	response, err := builder.Deploy(ctx, restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues()))
	requireRefused(t, response, err, refusal)
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	require.Empty(t, entries)

	built := loadedBuilder(t)
	built.Settings.MigrationSources = []MigrationSource{{Name: "events"}}
	buildResponse, err := built.Build(ctx, dockerBuildRequest(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_ERROR, buildResponse.GetState().GetState())
	require.Contains(t, buildResponse.GetState().GetMessage(), refusal)

	// Without migrations there is no Job to be partial.
	builder.Settings.NoMigration = true
	response, err = builder.Deploy(ctx, restrictedRenderRequest(t.TempDir(), builder, ownSecretsWithoutValues(), rootPlainValues()))
	requireDeployed(t, response, err)
}

// A restricted render refuses any image that is not digest-pinned; a
// docker-image override without one is named before anything renders.
func TestRestrictedDeploymentRefusesDigestlessImageOverride(t *testing.T) {
	ctx := context.Background()
	const digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	for override, refusal := range map[string]string{
		"clickhouse/clickhouse-server:26.3":         `clickhouse docker-image "clickhouse/clickhouse-server:26.3" has no digest`,
		"clickhouse/clickhouse-server":              `clickhouse docker-image "clickhouse/clickhouse-server" has no digest`,
		"clickhouse/clickhouse-server@sha256:short": "the digest must be sha256:<64 lower-case hex>",
	} {
		t.Run(override, func(t *testing.T) {
			builder := restrictedRenderBuilder(t)
			builder.Settings.Image = override
			destination := t.TempDir()
			response, err := builder.Deploy(ctx, restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues()))
			requireRefused(t, response, err, refusal)
			entries, err := os.ReadDir(destination)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}

	builder := restrictedRenderBuilder(t)
	builder.Settings.Image = "registry.example.com:5000/clickhouse/clickhouse-server:26.3@" + digest
	destination := t.TempDir()
	response, err := builder.Deploy(ctx, restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues()))
	requireDeployed(t, response, err)
	require.Equal(t, "registry.example.com:5000/clickhouse/clickhouse-server@"+digest,
		readWorkload(t, destination, "stateful-set.yaml").Spec.Template.Spec.Containers[0].Image)

	// The ephemeral profile still takes a tag-only override.
	builder.Settings.Image = "clickhouse/clickhouse-server:26.3"
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	response, err = builder.Deploy(ctx, renderRequest(t.TempDir(), builder, ephemeral, ownSecrets("evidence", "s3cr3t")))
	requireDeployed(t, response, err)
}

func TestParseImageOverride(t *testing.T) {
	const digest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	for reference, want := range map[string]string{
		"clickhouse/clickhouse-server:26.3":                         "clickhouse/clickhouse-server:26.3",
		"clickhouse/clickhouse-server":                              "clickhouse/clickhouse-server:latest",
		"clickhouse/clickhouse-server@" + digest:                    "clickhouse/clickhouse-server@" + digest,
		"clickhouse/clickhouse-server:26.3@" + digest:               "clickhouse/clickhouse-server@" + digest,
		"registry:5000/clickhouse/clickhouse-server":                "registry:5000/clickhouse/clickhouse-server:latest",
		"registry:5000/clickhouse/clickhouse-server:26.3":           "registry:5000/clickhouse/clickhouse-server:26.3",
		"registry:5000/clickhouse/clickhouse-server:26.3@" + digest: "registry:5000/clickhouse/clickhouse-server@" + digest,
	} {
		parsed, err := parseImageOverride(reference)
		require.NoError(t, err, reference)
		require.Equal(t, want, parsed.FullName(), reference)
	}
	for _, reference := range []string{
		"", "name:", "@" + digest, "name@sha256:XYZ", "name@" + strings.ToUpper(digest),
		// whitespace anywhere, including where TrimSpace would have hidden it
		" name:tag", "name:tag ", "a/b:c d", "name\t:tag",
		// empty or malformed repository components
		":tag", "/name", "registry:5000/", "a//b", "-x", "UPPER/Name:tag",
		// a ":" outside the registry host, a non-numeric registry port
		"name:tag:extra", "name:t/x", "garbage::",
		// tags outside the reference grammar
		"name:-tag", "name:" + strings.Repeat("t", 129), "name:t@g",
		"name@" + digest + "@" + digest,
	} {
		_, err := parseImageOverride(reference)
		require.Error(t, err, "%q", reference)
	}
}

// templateParameters are the parameters Prepare fills, for tests that render
// the templates directly (core's AssertKustomizeTemplates).
func templateParameters(withMigration bool, managedImage string) DeploymentTemplateParameters {
	return DeploymentTemplateParameters{
		WithMigration:    withMigration,
		ManagedImage:     managedImage,
		DatabaseName:     "example",
		Host:             "example-service.codefly-test.svc.cluster.local",
		Port:             9000,
		MigrationsDigest: strings.Repeat("0", 64),
		JobName:          "example-service-000000000000",
	}
}

// One Builder serves concurrent Deploys. Each render must carry the
// credentials of its own request: they are read into locals, never through
// fields another Deploy is writing (run with -race).
func TestConcurrentEphemeralDeploysKeepTheirOwnCredentials(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	users := []string{"alice", "bob"}
	for iteration := 0; iteration < 25; iteration++ {
		destinations := []string{t.TempDir(), t.TempDir()}
		var group sync.WaitGroup
		for i := range users {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				response, err := builder.Deploy(ctx, renderRequest(destinations[i], builder, ephemeral, ownSecrets(users[i], "pw-"+users[i])))
				assert.NoError(t, err)
				assert.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
				connection, err := resources.GetConfigurationValue(ctx, response.GetConfiguration(), "clickhouse", "connection")
				assert.NoError(t, err)
				assert.Contains(t, connection, "clickhouse://"+users[i]+":pw-"+users[i]+"@")
			}(i)
		}
		group.Wait()
		for i, user := range users {
			content, err := os.ReadFile(filepath.Join(destinations[i], "overlays", "staging", "secret.yaml"))
			require.NoError(t, err)
			var secret struct {
				Data map[string]string `yaml:"data"`
			}
			require.NoError(t, yaml.Unmarshal(content, &secret))
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte(user)), secret.Data["CLICKHOUSE_USER"], "iteration %d", iteration)
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte("pw-"+user)), secret.Data["CLICKHOUSE_PASSWORD"], "iteration %d", iteration)
		}
	}
}

// database-name is created by the image, dialled by the Job and named by the
// connection; outside the identifier charset it would need quoting in all
// three, and empty it silently means `default`. Refused before any write.
func TestDeploymentRefusesAnInvalidDatabaseName(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"", `a"b`, `a\b`, "a\nb", "my-db", "1db", "db name"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			builder := restrictedRenderBuilder(t)
			builder.Settings.DatabaseName = name
			destination := t.TempDir()
			response, err := builder.Deploy(ctx, restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues()))
			requireRefused(t, response, err, "clickhouse database-name")
			entries, err := os.ReadDir(destination)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

// The port, the host the Job dials and the connection consumers read come from
// one instance field each; an instance that contradicts itself, has no port,
// or takes the Service's HTTP port is refused by name.
func TestDeploymentRefusesAnUnusableInstancePort(t *testing.T) {
	ctx := context.Background()
	builder := restrictedRenderBuilder(t)
	for name, mutate := range map[string]func(*basev0.NetworkInstance){
		"port 0": func(instance *basev0.NetworkInstance) { instance.Port = 0 },
		"port 8123": func(instance *basev0.NetworkInstance) {
			instance.Port, instance.Address = 8123, "evidence.platform-obin-tracing.svc.cluster.local:8123"
		},
		"address port":       func(instance *basev0.NetworkInstance) { instance.Port = 31234 },
		"address host":       func(instance *basev0.NetworkInstance) { instance.Hostname = "elsewhere" },
		"unparsable address": func(instance *basev0.NetworkInstance) { instance.Address = "no-port" },
	} {
		t.Run(name, func(t *testing.T) {
			destination := t.TempDir()
			req := restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues())
			mutate(req.GetNetworkMappings()[0].GetInstances()[0])
			response, err := builder.Deploy(ctx, req)
			requireRefused(t, response, err, "clickhouse")
			require.Contains(t, response.GetState().GetMessage(), "instance")
			entries, err := os.ReadDir(destination)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

// renderedJobTemplateDigest is the digest of the Job's pod template as the
// rendered tree finally carries it, encoded the way the name was derived.
func renderedJobTemplateDigest(t *testing.T, destination string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(destination, "base", "job.yaml"))
	require.NoError(t, err)
	var job struct {
		Spec struct {
			Template yaml.Node `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(content, &job))
	encoded, err := yaml.Marshal(&job.Spec.Template)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])[:12]
}

// The Job's name is the service name plus the digest of the pod template the
// tree finally carries (anything core binds after the agent renders would
// show here), and that template carries the migrations' digest: the same
// inputs keep the name, a changed migration or a changed image makes a new
// Job, in both profiles.
func TestMigrationJobNameFollowsItsRenderedTemplateAndMigrations(t *testing.T) {
	ctx := context.Background()
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	for _, profile := range []string{"restricted", "ephemeral"} {
		t.Run(profile, func(t *testing.T) {
			builder := restrictedRenderBuilder(t)
			builder.Location = t.TempDir()
			migrations := filepath.Join(builder.Location, "migrations")
			require.NoError(t, os.MkdirAll(migrations, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(migrations, "1_init.up.sql"), []byte("CREATE TABLE a (x UInt8) ENGINE = Memory"), 0o644))
			render := func(digest string) string {
				destination := t.TempDir()
				var req *builderv0.DeploymentRequest
				if profile == "restricted" {
					req = restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues())
				} else {
					req = renderRequest(destination, builder, ephemeral, ownSecrets("evidence", "s3cr3t"))
				}
				req.GetDeployment().GetKubernetes().GetBuildContext().ImageDigest = digest
				response, err := builder.Deploy(ctx, req)
				requireDeployed(t, response, err)
				job := readWorkload(t, destination, "job.yaml")
				require.Regexp(t, migrationJobName, job.Metadata.Name)
				require.Equal(t, "evidence-"+renderedJobTemplateDigest(t, destination), job.Metadata.Name,
					"the name is the digest of the pod template the tree carries")
				return job.Metadata.Name
			}
			first := render(renderImageDigest)
			require.Equal(t, first, render(renderImageDigest), "same inputs, same Job")

			require.NoError(t, os.WriteFile(filepath.Join(migrations, "2_more.up.sql"), []byte("CREATE TABLE b (x UInt8) ENGINE = Memory"), 0o644))
			second := render(renderImageDigest)
			require.NotEqual(t, first, second, "a new migration under the same image ref is a new Job")

			require.NoError(t, os.WriteFile(filepath.Join(migrations, "2_more.up.sql"), []byte("CREATE TABLE c (x UInt8) ENGINE = Memory"), 0o644))
			third := render(renderImageDigest)
			require.NotEqual(t, second, third, "an edited migration is a new Job")

			require.NotEqual(t, third, render("sha256:"+strings.Repeat("b", 64)), "a new image is a new Job")
		})
	}
}

// One Builder renders the ephemeral profile (holding the values) and then the
// restricted one: nothing from the first may reach the second's tree or
// response.
func TestRestrictedRenderCarriesNoCredentialFromAnEarlierEphemeralRender(t *testing.T) {
	ctx := context.Background()
	const sentinelUser, sentinelPassword = "sentinel-user-ephemeral", "s3cr3t-ephemeral"
	builder := restrictedRenderBuilder(t)
	ephemeral := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	first := t.TempDir()
	response, err := builder.Deploy(ctx, renderRequest(first, builder, ephemeral, ownSecrets(sentinelUser, sentinelPassword)))
	requireDeployed(t, response, err)
	require.Contains(t, readRenderedTree(t, first), base64.StdEncoding.EncodeToString([]byte(sentinelPassword)), "the sentinel is live: the ephemeral tree holds it")

	destination := t.TempDir()
	response, err = builder.Deploy(ctx, restrictedRenderRequest(destination, builder, ownSecretsWithoutValues(), rootPlainValues()))
	requireDeployed(t, response, err)
	tree := readRenderedTree(t, destination)
	for _, sentinel := range []string{sentinelUser, sentinelPassword} {
		for _, form := range []string{sentinel, base64.StdEncoding.EncodeToString([]byte(sentinel)), escapeUserinfo(sentinel)} {
			require.NotContains(t, tree, form)
			require.NotContains(t, response.String(), form)
		}
	}
}
