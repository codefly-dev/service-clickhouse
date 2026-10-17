package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// The deployed engine is given the credentials and the database this service
// declares, under the variable names the IMAGE reads. It used to receive only
// the codefly carriers — which name a module and a service, and which
// clickhouse parses not at all — so it came up on its built-in `default` user
// and never created this service's database: the connection handed to every
// consumer, and to the bootstrap Job, was accepted by no server.
//
// Each profile delivers the values the only way it may, so both are rendered.
func TestDeployedEngineIsGivenItsCredentialsAndDatabase(t *testing.T) {
	ctx := context.Background()

	t.Run("restricted aliases the references the host declared", func(t *testing.T) {
		useSuccessfulKubectl(t)
		builder := loadedBuilder(t)
		builder.Base.SetDockerImage(image)
		destination := t.TempDir()
		requireRendered(ctx, t, builder, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1, destination)

		engine := readManifest(t, destination, "base", "stateful-set.yaml")
		// Aliased from the carrier references, so one declaration serves the
		// engine and the Job and no credential value is rendered.
		for _, expected := range []string{
			"- name: " + clickhouseUserVariable + "\n              valueFrom:\n                secretKeyRef:\n                  name: clickhouse-credentials\n                  key: user",
			"- name: " + clickhousePasswordVariable + "\n              valueFrom:\n                secretKeyRef:\n                  name: clickhouse-credentials\n                  key: password",
			"- name: " + clickhouseDatabaseVariable + "\n              value: \"" + builder.Settings.DatabaseName + "\"",
			"- name: " + clickhouseAlwaysInitVariable + "\n              value: \"1\"",
		} {
			require.Containsf(t, engine, expected, "the engine must be given %q:\n%s", expected, engine)
		}
	})

	t.Run("inline secrets deliver the declared values in the Secret", func(t *testing.T) {
		useSuccessfulKubectl(t)
		builder := loadedBuilder(t)
		builder.Base.SetDockerImage(image)
		destination := t.TempDir()
		requireRendered(ctx, t, builder, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1, destination)

		engine := readManifest(t, destination, "base", "stateful-set.yaml")
		require.Contains(t, engine, "- name: "+clickhouseDatabaseVariable+"\n              value: \""+builder.Settings.DatabaseName+"\"")
		require.Contains(t, engine, "envFrom:", "this profile loads the credentials from the Secret it renders")
		require.NotContains(t, engine, "secretKeyRef:", "there is no reference to alias in this profile")

		secret := readManifest(t, destination, "overlays", "test", "secret.yaml")
		for variable, value := range map[string]string{clickhouseUserVariable: "user", clickhousePasswordVariable: "password"} {
			require.Containsf(t, secret, variable+": "+base64.StdEncoding.EncodeToString([]byte(value)),
				"the Secret the engine loads must carry %s as the image reads it:\n%s", variable, secret)
		}
	})

	// A restricted render that declares neither reference is refused BY NAME.
	// Rendering it anyway produces an engine on its built-in `default` user,
	// and the first sign of that is an authentication failure somewhere else.
	t.Run("restricted without references is refused by name", func(t *testing.T) {
		useSuccessfulKubectl(t)
		builder := loadedBuilder(t)
		builder.Base.SetDockerImage(image)
		response, err := deployRender(ctx, t, builder,
			builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1, t.TempDir(),
			map[string]*builderv0.KubernetesSecretKeyReference{
				// The image's own names, which the Job's entrypoint cannot
				// read: a carrier reference is what both containers need.
				clickhouseUserVariable:     {Name: "clickhouse-credentials", Key: "user"},
				clickhousePasswordVariable: {Name: "clickhouse-credentials", Key: "password"},
			})
		require.NoError(t, err)
		require.Equal(t, builderv0.DeploymentStatus_ERROR, response.GetState().GetState())
		require.Contains(t, response.GetState().GetMessage(), builder.migrationUserKey())
		require.Contains(t, response.GetState().GetMessage(), builder.migrationPasswordKey())
	})
}

func readManifest(t *testing.T, destination string, parts ...string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{destination}, parts...)...))
	require.NoError(t, err)
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}
