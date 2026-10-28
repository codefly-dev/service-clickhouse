package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// TestBuildRecipeClearsStaleOutput locks the reproducibility invariant: a
// reused output_directory carrying files from a prior build must not leak into
// the recipe. Because BuildDockerBuildPlan inventories whatever is on disk and
// the CLI verifies that same tree, a stale file would be digested into the plan
// and baked into the image while still passing verification — silent
// contamination. The build must own its tree and clear it first.
func TestBuildRecipeClearsStaleOutput(t *testing.T) {
	ctx := context.Background()
	builder := loadedBuilder(t)

	output := t.TempDir()
	stale := filepath.Join(output, "migrations", "9999_stale.up.sql")
	require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0o755))
	require.NoError(t, os.WriteFile(stale, []byte("SELECT 'stale'"), 0o644))
	staleTop := filepath.Join(output, "builder", "leftover.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(staleTop), 0o755))
	require.NoError(t, os.WriteFile(staleTop, []byte("leftover"), 0o644))

	resp, err := builder.Build(ctx, dockerBuildRequest(output))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	require.NoFileExists(t, stale, "stale migration must not survive into the recipe tree")
	require.NoFileExists(t, staleTop, "stale builder artifact must not survive into the recipe tree")

	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan)
	for _, file := range plan.GetFiles() {
		require.NotContains(t, file.GetPath(), "9999_stale", "stale file leaked into the plan inventory")
		require.NotContains(t, file.GetPath(), "leftover", "stale file leaked into the plan inventory")
	}
	require.NoError(t, services.VerifyDockerBuildPlan(output, plan))
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

// Exercise the rendered recipe and every resulting layer: deleting the README
// in a later RUN would still leave its example credentials in the image.
func TestMigrationImageContainsOnlyBuiltExecutable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	builder := loadedBuilder(t)
	output := t.TempDir()
	resp, err := builder.Build(ctx, dockerBuildRequest(output))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
	recipe := resp.GetResult().GetDockerBuildPlan().GetRecipes()[0]
	image := "codefly-clickhouse-archive-test:" + strings.ToLower(filepath.Base(t.TempDir()))
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", image).Run() })
	build := exec.CommandContext(ctx, "docker", "build", "--tag", image, "--file", filepath.Join(output, recipe.GetDockerfile()), output)
	log, err := build.CombinedOutput()
	require.NoError(t, err, "%s", log)
	version, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "/usr/local/bin/migrate", image, "-version").CombinedOutput()
	require.NoError(t, err, "%s", version)
	expectedVersion, err := linkedMigrateRelease()
	require.NoError(t, err)
	require.Equal(t, expectedVersion, strings.TrimSpace(string(version)))
	help, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "/usr/local/bin/migrate", image, "-help").CombinedOutput()
	require.NoError(t, err, "%s", help)
	require.Contains(t, string(help), "clickhouse")
	require.Contains(t, string(help), "Source drivers: file")
	require.NotContains(t, string(help), "postgres")
	archive := filepath.Join(t.TempDir(), "image.tar")
	log, err = exec.CommandContext(ctx, "docker", "save", "--output", archive, image).CombinedOutput()
	require.NoError(t, err, "%s", log)
	file, err := os.Open(archive)
	require.NoError(t, err)
	defer file.Close()
	outer := tar.NewReader(file)
	// Both Docker's legacy and OCI-backed stores include manifest.json, whose
	// Layers names select the actual tar blobs (not config or attestation JSON).
	type manifest struct{ Layers []string }
	var manifests []manifest
	for {
		h, err := outer.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Name == "manifest.json" {
			require.NoError(t, json.NewDecoder(outer).Decode(&manifests))
		}
	}
	require.Len(t, manifests, 1)
	require.NotEmpty(t, manifests[0].Layers)
	wanted := make(map[string]bool)
	for _, layer := range manifests[0].Layers {
		wanted[layer] = true
	}
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	outer = tar.NewReader(file)
	layers := 0
	executable := false
	for {
		h, err := outer.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if !wanted[h.Name] {
			continue
		}
		layers++
		blob := bufio.NewReader(outer)
		magic, err := blob.Peek(2)
		require.NoError(t, err)
		var content io.Reader = blob
		if magic[0] == 0x1f && magic[1] == 0x8b {
			compressed, err := gzip.NewReader(blob)
			require.NoError(t, err)
			defer compressed.Close()
			content = compressed
		}
		layer := tar.NewReader(content)
		for {
			file, err := layer.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			name := strings.TrimPrefix(file.Name, "./")
			require.NotEqual(t, "app/README.md", name, "upstream README must not occur in any layer")
			require.False(t, strings.HasPrefix(name, "app/builder/"), "build context must not enter runtime layers")
			require.False(t, strings.HasPrefix(name, "usr/local/go/"), "compiler must not enter runtime layers")
			if name == "usr/local/bin/migrate" {
				executable = true
			}
		}
	}
	require.Equal(t, len(wanted), layers, "must inspect every image layer")
	require.True(t, executable, "must inspect the installed migrate executable")
}
