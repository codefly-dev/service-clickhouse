package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The migration Job used to render under the service's own name with
// `ttlSecondsAfterFinished: 0`. Kubernetes deleted it the moment it completed,
// so a GitOps reconciler holding the rendered manifest reported the Job
// missing forever; and because a Job's pod template is immutable, a changed
// migration image could never be applied under that same name anyway.
//
// The Job is now named after its content, the way service-postgres names its
// bootstrap Job: the service name plus a digest of the rendered pod template.
// A restricted render pins the migration image by digest, and that image is
// built from `migrations/`, so the digest the build hands Deploy is what
// carries a changed migration set into the render.

const (
	migrationsDigest        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	changedMigrationsDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// renderMigrationJob renders the restricted manifest tree for the migration
// image the build produced at digest, after vary has adjusted the request, and
// returns the Job's manifest.
func renderMigrationJob(t *testing.T, builder *Builder, digest string, vary func(*builderv0.DeploymentRequest)) string {
	t.Helper()
	destination := t.TempDir()
	request := deployRequest(t, builder, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1, destination, nil)
	request.GetDeployment().GetKubernetes().BuildContext = &builderv0.DockerBuildContext{
		DockerRepository: "registry.example.com",
		ImageDigest:      digest,
	}
	if vary != nil {
		vary(request)
	}
	response, err := builder.Deploy(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	job, err := os.ReadFile(filepath.Join(destination, "base", "job.yaml"))
	require.NoError(t, err)
	return string(job)
}

func migrationJobName(t *testing.T, manifest string) string {
	t.Helper()
	var job struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(manifest), &job))
	require.NotEmpty(t, job.Metadata.Name)
	return job.Metadata.Name
}

func TestMigrationJobIsNamedAfterItsContent(t *testing.T) {
	useSuccessfulKubectl(t)
	builder := loadedBuilder(t)

	baseline := renderMigrationJob(t, builder, migrationsDigest, nil)
	name := migrationJobName(t, baseline)
	require.Regexp(t, "^"+regexp.QuoteMeta(builder.Identity.Name)+"-[0-9a-f]{12}$", name)
	require.Contains(t, baseline, "@"+migrationsDigest, "the Job must run the migration image the build produced")

	// An unchanged render names the same Job, so a reconciler leaves the
	// completed one alone instead of running the migrations again.
	require.Equal(t, name, migrationJobName(t, renderMigrationJob(t, builder, migrationsDigest, nil)))

	// A changed migration set is a new image, which names a new Job: the old
	// one's immutable pod template is never patched, the new one runs.
	require.NotEqual(t, name, migrationJobName(t, renderMigrationJob(t, builder, changedMigrationsDigest, nil)))

	// The name is the digest of exactly the pod template the render wrote, so
	// it cannot drift from what the Job runs.
	var rendered struct {
		Spec struct {
			Template yaml.Node `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(baseline), &rendered))
	podTemplate, err := yaml.Marshal(&rendered.Spec.Template)
	require.NoError(t, err)
	sum := sha256.Sum256(podTemplate)
	require.Equal(t, builder.Identity.Name+"-"+hex.EncodeToString(sum[:])[:12], name)

	// What only the server reads leaves the Job alone, so it does not rerun
	// the migrations. (The image is held fixed here: in a real build the
	// database name is also baked into the migration image.)
	databaseName := builder.Settings.DatabaseName
	builder.Settings.DatabaseName = "renamed_database"
	require.Equal(t, name, migrationJobName(t, renderMigrationJob(t, builder, migrationsDigest, nil)))
	builder.Settings.DatabaseName = databaseName

	// Anything else the Job runs with names a new Job.
	for change, vary := range map[string]func(*builderv0.DeploymentRequest){
		"address": func(request *builderv0.DeploymentRequest) {
			for _, instance := range request.GetNetworkMappings()[0].GetInstances() {
				if strings.HasSuffix(instance.GetHostname(), ".svc.cluster.local") {
					instance.Address = "clickhouse.codefly-test.svc.cluster.local:9440"
					instance.Port = 9440
				}
			}
		},
		"credential reference": func(request *builderv0.DeploymentRequest) {
			for _, reference := range request.GetDeployment().GetKubernetes().GetSecretReferences() {
				reference.Name = "rotated-clickhouse-credentials"
			}
		},
	} {
		t.Run(change, func(t *testing.T) {
			require.NotEqual(t, name, migrationJobName(t, renderMigrationJob(t, builder, migrationsDigest, vary)))
		})
	}
}

// A completed Job must outlive its run: the manifest declaring it stays in
// sync only while the object exists. Neither profile may hand it a TTL.
func TestMigrationJobIsNotDeletedOnCompletion(t *testing.T) {
	for _, profile := range []builderv0.KubernetesOutputProfile{
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
	} {
		t.Run(profile.String(), func(t *testing.T) {
			ctx := context.Background()
			useSuccessfulKubectl(t)
			builder := loadedBuilder(t)
			builder.Base.SetDockerImage(image)
			destination := t.TempDir()
			requireRendered(ctx, t, builder, profile, destination)

			job, err := os.ReadFile(filepath.Join(destination, "base", "job.yaml"))
			require.NoError(t, err)
			var manifest struct {
				Spec map[string]any `yaml:"spec"`
			}
			require.NoError(t, yaml.Unmarshal(job, &manifest))
			require.NotContains(t, manifest.Spec, "ttlSecondsAfterFinished")
			require.Regexp(t, "^"+regexp.QuoteMeta(builder.Identity.Name)+"-[0-9a-f]{12}$", migrationJobName(t, string(job)))
		})
	}
}
