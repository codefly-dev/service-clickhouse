package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func TestManifestGuardRender(t *testing.T) {
	destination := os.Getenv("CODEFLY_MANIFEST_DESTINATION")
	if destination == "" {
		t.Skip("CODEFLY_MANIFEST_DESTINATION is unset; skipping manifest guard render")
	}
	environment := os.Getenv("CODEFLY_MANIFEST_ENVIRONMENT")
	namespace := os.Getenv("CODEFLY_MANIFEST_NAMESPACE")
	profileName := os.Getenv("CODEFLY_MANIFEST_PROFILE")
	require.NotEmpty(t, environment, "CODEFLY_MANIFEST_ENVIRONMENT is required")
	require.NotEmpty(t, namespace, "CODEFLY_MANIFEST_NAMESPACE is required")

	profile, ok := builderv0.KubernetesOutputProfile_value[profileName]
	require.Truef(t, ok, "unknown CODEFLY_MANIFEST_PROFILE %q", profileName)

	ctx := context.Background()
	identity := &resources.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "clickhouse",
		Version:   "0.0.0",
	}
	base := &services.Base{
		Wool:     wool.Get(ctx),
		Identity: identity,
		Information: &services.Information{
			Service: resources.ToServiceWithCase(identity),
			Module:  resources.ToModuleWithCase(identity),
		},
	}
	base.SetDockerImage(image)
	builder := &services.BuilderWrapper{Base: base}
	base.Builder = builder

	deployment := &builderv0.KubernetesDeployment{
		Namespace:   namespace,
		Destination: destination,
		Profile:     builderv0.KubernetesOutputProfile(profile),
		SecretReferences: map[string]*builderv0.KubernetesSecretKeyReference{
			"CLICKHOUSE_USER":     {Name: "clickhouse", Key: "user"},
			"CLICKHOUSE_PASSWORD": {Name: "clickhouse", Key: "password"},
		},
	}
	params := services.DeploymentParameters{
		SecretReferences: deployment.GetSecretReferences(),
		Parameters: DeploymentTemplateParameters{
			WithMigration: true,
			ManagedImage:  image.FullName(),
		},
	}

	err := builder.KustomizeDeploy(ctx, &basev0.Environment{Name: environment}, deployment, deploymentFS, params)
	require.NoError(t, err)
	validation := services.ValidateKubernetesManifestTree(
		ctx,
		destination,
		environment,
		namespace,
		builderv0.KubernetesOutputProfile(profile),
		false,
		"",
		"",
	)
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, validation.GetStaticValidation(), validation.GetViolations())
	require.True(t, validation.GetRestricted())

	statefulSet, err := os.ReadFile(filepath.Join(destination, "base", "stateful-set.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(statefulSet), "secretKeyRef:")
	require.NotContains(t, string(statefulSet), "envFrom:")
	// The workload references its secrets by name, so there is no inline secret
	// material to render. core skips a manifest whose render is blank rather than
	// leaving an empty stub in the tree (which would be digested into the signed
	// promotion bundle), so the overlay is either absent or empty — both satisfy
	// "no inline secret material".
	secret, err := os.ReadFile(filepath.Join(destination, "overlays", environment, "secret.yaml"))
	if err == nil {
		require.Empty(t, strings.TrimSpace(string(secret)))
	} else {
		require.True(t, os.IsNotExist(err), "unexpected error reading secret overlay: %v", err)
	}
}
