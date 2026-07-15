package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// TestCreateToRunDocker runs the full agent lifecycle against the Docker
// runtime (the default container backend).
func TestCreateToRunDocker(t *testing.T) {
	testCreateToRun(t, resources.NewRuntimeContextFree())
}

// testCreateToRun drives Load → Create → Init → Start → connect → SELECT 1 +
// applies the scaffolded migration, so docker and nix exercise the identical
// agent path. The test connects with the agent's OWN native tcp:// DSN
// (runtime.connection) — the clickhouse-go v1 driver registered in this module
// speaks tcp://, while the exported `connection` value is the clickhouse:// URL
// for external v2/JDBC consumers.
func testCreateToRun(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}

	tmpDir := t.TempDir()
	defer func(p string) { _ = os.RemoveAll(p) }(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	err := service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name))
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	builder := NewBuilder()
	resp, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	runtime := NewRuntime()

	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	env := resources.LocalEnvironment()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)
	require.Equal(t, 1, len(runtime.Endpoints))
	defer func() {
		resp, destroyErr := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
		if destroyErr != nil {
			t.Errorf("destroy ClickHouse test runtime: %v", destroyErr)
			return
		}
		if destroyErr = services.ValidateRuntimeDestroyResponse(resp); destroyErr != nil {
			t.Errorf("destroy ClickHouse test runtime: %v", destroyErr)
		}
	}()

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints)
	require.NoError(t, err)
	require.Equal(t, 1, len(networkMappings))

	conf := &basev0.Configuration{
		Origin:         fmt.Sprintf("mod/%s", service.Name),
		RuntimeContext: resources.NewRuntimeContextFree(),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "CLICKHOUSE_USER", Value: "clickhouse"},
					{Key: "CLICKHOUSE_PASSWORD", Value: "password"},
				},
			},
		},
	}

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		Configuration:           conf,
		ProposedNetworkMappings: networkMappings,
	})
	require.NoError(t, err)
	require.NotNil(t, init)
	require.NoError(t, services.ValidateRuntimeInitResponse(init))
	require.Len(t, runtime.Runtime.RuntimeConfigurations, 2)
	require.Len(t, init.RuntimeConfigurations, 2)

	start, err := runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.NoError(t, services.ValidateRuntimeStartResponse(start))

	// The exported consumer connection is the modern clickhouse:// URL.
	configurationOut, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	require.NotNil(t, configurationOut, "runtime configurations: %v", init.RuntimeConfigurations)
	connString, err := resources.GetConfigurationValue(ctx, configurationOut, "clickhouse", "connection")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(connString, "clickhouse://"), "exported connection should be a clickhouse:// URL, got %q", connString)

	// Live query via the agent's native tcp:// DSN (clickhouse-go v1 driver).
	db, err := sql.Open("clickhouse", runtime.connection)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Ping())
	_, err = db.Exec("SELECT 1")
	require.NoError(t, err)

	// The scaffolded migration (1_create_table) must have been applied during
	// Init/Start — its tracking table is the default schema_migrations.
	var version uint64
	var dirty bool
	err = db.QueryRow("SELECT version, dirty FROM schema_migrations ORDER BY sequence DESC LIMIT 1").Scan(&version, &dirty)
	require.NoError(t, err, "schema_migrations should exist after migrations ran")
	require.False(t, dirty, "migration left dirty")
	require.GreaterOrEqual(t, version, uint64(1), "expected at least migration 1 applied")
}
