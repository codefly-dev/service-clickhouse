package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// loadedBuilder drives Load → Create so the service dir carries the scaffolded
// migrations the build recipe copies into the caller-owned output directory.
func loadedBuilder(t *testing.T) *Builder {
	t.Helper()
	ctx := context.Background()

	tmpDir := t.TempDir()
	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	require.NoError(t, service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name)))

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           "test",
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)
	return builder
}

func dockerBuildRequest(output string) *builderv0.BuildRequest {
	return &builderv0.BuildRequest{
		OutputDirectory: output,
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
			},
		},
	}
}

// TestBuildEmitsRecipePlan locks the CLI-owned build contract: when the CLI
// supplies output_directory, the agent renders a self-contained recipe tree
// there and returns a DockerBuildPlan the CLI can verify and build multi-arch —
// it does not run docker itself.
func TestBuildEmitsRecipePlan(t *testing.T) {
	ctx := context.Background()
	builder := loadedBuilder(t)

	output := t.TempDir()
	resp, err := builder.Build(ctx, dockerBuildRequest(output))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan, "expected a DockerBuildPlan, got %T", resp.GetResult().GetKind())
	require.Len(t, plan.GetRecipes(), 1)

	recipe := plan.GetRecipes()[0]
	require.Equal(t, "migration", recipe.GetName())
	require.Equal(t, "builder/Dockerfile", recipe.GetDockerfile())
	require.Equal(t, ".", recipe.GetContext())
	require.NotEmpty(t, recipe.GetImage())
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms())

	// The CLI verifies the on-disk tree against the plan before running buildx.
	require.NoError(t, services.VerifyDockerBuildPlan(output, plan))

	// The rendered recipe is self-contained: Dockerfile plus the migrations the
	// image applies.
	dockerfile, err := os.ReadFile(filepath.Join(output, "builder", "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "TARGETARCH", "Dockerfile must resolve the migrate binary per target arch for a multi-arch build")
	require.NotContains(t, string(dockerfile), "migrate.linux-amd64", "arch must not be hardcoded")

	entries, err := os.ReadDir(filepath.Join(output, "migrations"))
	require.NoError(t, err)
	var hasUp bool
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".up.sql") {
			hasUp = true
		}
	}
	require.True(t, hasUp, "scaffolded migration must be copied into the recipe context: %v", entries)
}

// TestBuildNoMigrationSkipsRecipe confirms a service with migrations disabled
// emits no build result even when the CLI asks for a recipe.
func TestBuildNoMigrationSkipsRecipe(t *testing.T) {
	ctx := context.Background()
	builder := loadedBuilder(t)
	builder.Settings.NoMigration = true

	resp, err := builder.Build(ctx, dockerBuildRequest(t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState())
	require.Nil(t, resp.GetResult())
}
