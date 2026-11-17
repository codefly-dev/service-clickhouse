package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
)

// useTestStore points the agent's native state at a store of the test's own,
// laid out as the machine's is: <root>/clickhouse/<run>. It returns the root,
// which stands for runstate.Root().
func useTestStore(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "codefly")
	previous := nativeStore
	nativeStore = func() string { return filepath.Join(root, "clickhouse") }
	t.Cleanup(func() { nativeStore = previous })
	return root
}

func requireNix(t *testing.T) {
	t.Helper()
	if !runners.CheckNixInstalled() || !runners.IsNixSupported() {
		t.Skip("nix not installed/supported on this host")
	}
}

// nativeFixture is one scaffolded clickhouse service in a workspace of its own.
type nativeFixture struct {
	workspace *resources.Workspace
	identity  *basev0.ServiceIdentity
}

func newNativeFixture(t *testing.T, ctx context.Context) *nativeFixture {
	t.Helper()
	dir := t.TempDir()
	name := fmt.Sprintf("svc-%v", time.Now().UnixNano())
	service := resources.Service{Name: name, Version: "test-me"}
	require.NoError(t, service.SaveAtDir(ctx, path.Join(dir, "mod", name)))
	fixture := &nativeFixture{
		workspace: &resources.Workspace{Name: "test"},
		identity: &basev0.ServiceIdentity{
			Name:                name,
			Module:              "mod",
			Workspace:           "test",
			WorkspacePath:       dir,
			RelativeToWorkspace: "mod/" + name,
		},
	}
	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: fixture.identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)
	return fixture
}

// load is the service's agent as a run under scope loads it; disposable is
// what that run's caller said of its state.
func (f *nativeFixture) load(t *testing.T, ctx context.Context, scope string, disposable bool) *Runtime {
	t.Helper()
	environment := shared.Must(resources.LocalEnvironment().Proto())
	environment.NamingScope = scope
	environment.Disposable = disposable
	runtime := NewRuntime()
	_, err := runtime.Load(ctx, &runtimev0.LoadRequest{Identity: f.identity, Environment: environment, DisableCatch: true})
	require.NoError(t, err)
	return runtime
}

// initNative starts the service natively, on a port of its own.
func (f *nativeFixture) initNative(t *testing.T, ctx context.Context, runtime *Runtime) {
	t.Helper()
	manager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	manager.WithTemporaryPorts()
	context := resources.NewRuntimeContextNix()
	mappings, err := manager.GenerateNetworkMappings(ctx, resources.LocalEnvironment(), f.workspace, runtime.Identity, runtime.Endpoints, context)
	require.NoError(t, err)
	response, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          context,
		ProposedNetworkMappings: mappings,
		Configuration: &basev0.Configuration{
			Origin:         f.identity.RelativeToWorkspace,
			RuntimeContext: resources.NewRuntimeContextFree(),
			Infos: []*basev0.ConfigurationInformation{{
				Name: "clickhouse",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "CLICKHOUSE_USER", Value: "clickhouse"},
					{Key: "CLICKHOUSE_PASSWORD", Value: "password"},
				},
			}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, services.ValidateRuntimeInitResponse(response))
}

func destroy(t *testing.T, ctx context.Context, runtime *Runtime) {
	t.Helper()
	response, err := runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	require.NoError(t, services.ValidateRuntimeDestroyResponse(response))
}

// claimedState is the state directory of a service whose agent initialized it
// natively and holds no server: what Destroy sees once the server is stopped.
func claimedState(t *testing.T, runtime *Runtime) *nixClickhouse {
	t.Helper()
	run := runstate.For(runtime.Identity.WorkspacePath, runtime.Environment)
	root, err := nixRuntimeRoot(runtime.Location, run.NamingScope)
	require.NoError(t, err)
	state, err := claimNativeState(root, run)
	require.NoError(t, err)
	data := filepath.Join(root, "chdata")
	require.NoError(t, os.MkdirAll(data, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "rows"), []byte("kept"), 0o600))
	return &nixClickhouse{root: root, state: state, dataDir: data}
}

// TestDestroyRemovesADisposableRunsNativeState: a run whose caller declared
// its state disposable (a generated naming scope: temporary ports, a test's
// dependency stack) leaves no state directory behind once it is destroyed.
func TestDestroyRemovesADisposableRunsNativeState(t *testing.T) {
	useTestStore(t)
	ctx := context.Background()
	runtime := newNativeFixture(t, ctx).load(t, ctx, "invdisposable", true)
	runtime.nixRuntime = claimedState(t, runtime)
	root := runtime.nixRuntime.root

	stopped, err := runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StopStatus_SUCCESS, stopped.GetStatus().GetState(), stopped.GetStatus().GetMessage())
	require.FileExists(t, filepath.Join(root, "chdata", "rows"), "stop is not the end of a run")

	destroy(t, ctx, runtime)
	require.NoDirExists(t, root, "a disposable run must not leave its state directory behind")

	destroy(t, ctx, runtime)
}

// TestDestroyRetainsAPersistentRunsNativeState: a destroy is never read as
// permission to drop a developer's database. Only a run that has a naming
// scope and whose caller called it disposable gives that permission.
func TestDestroyRetainsAPersistentRunsNativeState(t *testing.T) {
	for name, run := range map[string]struct {
		scope      string
		disposable bool
	}{
		"a plain run":                           {},
		"a scope the developer named":           {scope: "review"},
		"an unscoped run said to be disposable": {disposable: true},
	} {
		t.Run(name, func(t *testing.T) {
			useTestStore(t)
			ctx := context.Background()
			runtime := newNativeFixture(t, ctx).load(t, ctx, run.scope, run.disposable)
			runtime.nixRuntime = claimedState(t, runtime)
			root := runtime.nixRuntime.root

			destroy(t, ctx, runtime)

			data, err := os.ReadFile(filepath.Join(root, "chdata", "rows"))
			require.NoError(t, err, "a persistent run's data must survive destroy")
			require.Equal(t, "kept", string(data))
		})
	}
}

// TestADisposableDirectoryIsInUseWhileItsServerRuns: the server is a process
// of its own and can outlive the agent that started it. While it runs the
// directory is in use, whoever else is gone.
func TestADisposableDirectoryIsInUseWhileItsServerRuns(t *testing.T) {
	root := filepath.Join(useTestStore(t), "clickhouse", "run")
	data := filepath.Join(root, "chdata")
	server := leftServer(t)
	// The agent that started it is killed: it never stops the server.
	abandonedStateWithServer(t, root, "invserver", server.Process.Pid)

	removed, err := runstate.Collect(root)
	require.NoError(t, err)
	require.False(t, removed, "the server still runs in the directory")
	require.DirExists(t, data)

	require.NoError(t, server.Process.Kill())
	_ = server.Wait()
	removed, err = runstate.Collect(root)
	require.NoError(t, err)
	require.True(t, removed)
}

// leftServer is a process standing for a clickhouse server its run left
// running. It is this test's own child, and is killed when the test ends.
func leftServer(t *testing.T) *exec.Cmd {
	t.Helper()
	server := exec.Command("sleep", "600")
	require.NoError(t, server.Start())
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	return server
}

func exited(server *exec.Cmd) bool {
	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// TestKilledDisposableRunsServerIsStoppedAtNextInit: a killed run leaves its
// server running, and a directory is in use for as long as it runs, so the
// directory was kept for good and the server with it. The next clickhouse
// agent to initialize natively on the machine stops the server such a run
// left and removes its directory; a developer's server, and the server of a
// run that still lives, are left exactly as they are.
func TestKilledDisposableRunsServerIsStoppedAtNextInit(t *testing.T) {
	store := filepath.Join(useTestStore(t), "clickhouse")
	killedServer, persistentServer, liveServer := leftServer(t), leftServer(t), leftServer(t)
	killed := abandonedStateWithServer(t, filepath.Join(store, "killed"), "invkilled", killedServer.Process.Pid)
	persistent := abandonedStateWithServer(t, filepath.Join(store, "persistent"), "", persistentServer.Process.Pid)
	// Its agent died, inside a run that goes on: this test is that run.
	live := crashedAgentState(t, filepath.Join(store, "live"), "invlive", liveServer.Process.Pid)

	_, err := claimNativeState(filepath.Join(store, "self"), runstate.Run{Workspace: "/work/shop", NamingScope: "invnext", Disposable: true})
	require.NoError(t, err)

	require.True(t, exited(killedServer), "the server a killed disposable run left must be stopped")
	require.NoDirExists(t, killed)
	require.False(t, processGone(persistentServer.Process.Pid), "a developer's server is theirs to stop")
	require.FileExists(t, filepath.Join(persistent, "chdata", "rows"))
	require.False(t, processGone(liveServer.Process.Pid), "a run that goes on restarts its agent and keeps its server")
	require.DirExists(t, live)
}

func TestTrackServerRefusesAServerThatWasNotStarted(t *testing.T) {
	root := filepath.Join(useTestStore(t), "clickhouse", "run")
	state, err := claimNativeState(root, runstate.Run{Workspace: "/work/shop"})
	require.NoError(t, err)
	n := &nixClickhouse{root: root, state: state, dataDir: filepath.Join(root, "chdata")}
	require.Error(t, n.trackServer())
}

const claimHelperDir = "CFCH_TEST_CLAIM_DIR"

// TestHelperClaimRunState is not a test: re-executed with claimHelperDir set,
// it claims that directory as an agent would, records the server it is told
// of, and exits. Told to launch, it is the run instead: it starts itself again
// as that agent and exits after it, which leaves the record of a run that was
// killed, agent and launcher both gone.
func TestHelperClaimRunState(t *testing.T) {
	dir := os.Getenv(claimHelperDir)
	if dir == "" {
		return
	}
	if os.Getenv(claimHelperDir+"_LAUNCH") != "" {
		agent := exec.Command(os.Args[0], "-test.run=^TestHelperClaimRunState$")
		agent.Env = append(os.Environ(), claimHelperDir+"_LAUNCH=")
		out, err := agent.CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	run := runstate.Run{Workspace: "/work/shop", NamingScope: os.Getenv(claimHelperDir + "_SCOPE")}
	run.Disposable = run.NamingScope != ""
	state, err := runstate.Claim(dir, run)
	require.NoError(t, err)
	if server := os.Getenv(claimHelperDir + "_SERVER"); server != "" {
		pid, err := strconv.Atoi(server)
		require.NoError(t, err)
		require.NoError(t, state.Track(pid))
	}
}

// abandonedState is the state directory of a run that was killed: its agent
// and the run that launched it are gone.
func abandonedState(t *testing.T, root, scope string) string {
	t.Helper()
	return claimedByAHelper(t, root, scope, 0, true)
}

// abandonedStateWithServer is abandonedState with the server the run's agent
// started there, still running.
func abandonedStateWithServer(t *testing.T, root, scope string, server int) string {
	t.Helper()
	return claimedByAHelper(t, root, scope, server, true)
}

// crashedAgentState is the state directory of an agent that died inside a run
// that goes on, with the server it started there. The run is this test.
func crashedAgentState(t *testing.T, root, scope string, server int) string {
	t.Helper()
	return claimedByAHelper(t, root, scope, server, false)
}

func claimedByAHelper(t *testing.T, root, scope string, server int, launcherGone bool) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "chdata"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "chdata", "rows"), []byte("kept"), 0o600))
	claim := exec.Command(os.Args[0], "-test.run=^TestHelperClaimRunState$")
	claim.Env = append(os.Environ(), claimHelperDir+"="+root, claimHelperDir+"_SCOPE="+scope)
	if server != 0 {
		claim.Env = append(claim.Env, claimHelperDir+"_SERVER="+strconv.Itoa(server))
	}
	if launcherGone {
		claim.Env = append(claim.Env, claimHelperDir+"_LAUNCH=1")
	}
	out, err := claim.CombinedOutput()
	require.NoError(t, err, string(out))
	return root
}

// TestKilledDisposableRunStateIsCollectedAtNextInit: a killed run never
// destroys its agents. The next clickhouse agent to initialize natively on the
// machine removes what such a run left, when that run was disposable and only
// then.
func TestKilledDisposableRunStateIsCollectedAtNextInit(t *testing.T) {
	store := filepath.Join(useTestStore(t), "clickhouse")
	disposable := abandonedState(t, filepath.Join(store, "disposable"), "invkilled")
	persistent := abandonedState(t, filepath.Join(store, "persistent"), "")
	unclaimed := filepath.Join(store, "unclaimed")
	require.NoError(t, os.MkdirAll(filepath.Join(unclaimed, "chdata"), 0o755))
	held := filepath.Join(store, "held")
	_, err := claimNativeState(held, runstate.Run{Workspace: "/work/shop", NamingScope: "invlive", Disposable: true})
	require.NoError(t, err)

	_, err = claimNativeState(filepath.Join(store, "self"), runstate.Run{Workspace: "/work/shop", NamingScope: "invnext", Disposable: true})
	require.NoError(t, err)

	require.NoDirExists(t, disposable)
	require.FileExists(t, filepath.Join(persistent, "chdata", "rows"), "a developer's database is never removed")
	require.DirExists(t, unclaimed, "a directory nobody claimed is nobody's to remove")
	require.DirExists(t, held, "a live agent still uses it")
	require.DirExists(t, filepath.Join(store, "self"))
}

// TestNativeStateIsWhereCodeflyClearLooks: `codefly clear` sweeps
// runstate.Root() and finds a state directory only within its depth.
func TestNativeStateIsWhereCodeflyClearLooks(t *testing.T) {
	machine, err := runstate.Root()
	require.NoError(t, err)
	suiteStore := nativeStore
	nativeStore = machineNativeStore
	root, err := nixRuntimeRoot("/workspace/modules/tracing/services/evidence", "invdepth")
	nativeStore = suiteStore
	require.NoError(t, err)
	relative, err := filepath.Rel(machine, root)
	require.NoError(t, err)
	require.Len(t, strings.Split(relative, string(filepath.Separator)), 2, "<root>/clickhouse/<run>, got %s", relative)
	require.Equal(t, "clickhouse", filepath.Dir(relative))

	swept := useTestStore(t)
	kept := abandonedState(t, filepath.Join(swept, "clickhouse", "kept"), "")
	killed := abandonedState(t, filepath.Join(swept, "clickhouse", "killed"), "invkilled")
	found, err := runstate.Sweep(swept, runstate.SweepOptions{})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, killed, found[0].Dir)
	require.NoDirExists(t, killed)
	require.DirExists(t, kept)
}

// serverOf is the clickhouse server process recorded in a state directory.
func serverOf(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "chdata", "status"))
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "PID:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			require.NoError(t, err)
			return pid
		}
	}
	t.Fatalf("no PID in %s", data)
	return 0
}

func processGone(pid int) bool {
	_, err := runners.InspectProcess(pid)
	return errors.Is(err, runners.ErrProcessNotFound)
}

// TestNixDisposableRunLeavesNoNativeState runs a real server (it needs nix and
// the clickhouse binary): Stop keeps the directory, because a service restarts
// inside a run, and Destroy stops the server and removes it.
func TestNixDisposableRunLeavesNoNativeState(t *testing.T) {
	requireNix(t)
	useTestStore(t)
	ctx := context.Background()
	fixture := newNativeFixture(t, ctx)
	runtime := fixture.load(t, ctx, "invdisposable", true)
	t.Cleanup(func() {
		if runtime.nixRuntime != nil {
			_ = runtime.nixRuntime.Stop(ctx)
		}
	})
	fixture.initNative(t, ctx, runtime)
	root := runtime.nixRuntime.root
	server := serverOf(t, root)
	require.False(t, processGone(server))
	// The process this agent started is the server itself, so the handle it
	// holds says which process to track: nothing forks away from it.
	started, err := runtime.nixRuntime.proc.(runners.HostProcess).Process()
	require.NoError(t, err)
	require.Equal(t, server, started.PID, "the server must be the process the agent started, not a child of it")

	stopped, err := runtime.Stop(ctx, &runtimev0.StopRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StopStatus_SUCCESS, stopped.GetStatus().GetState(), stopped.GetStatus().GetMessage())
	require.DirExists(t, filepath.Join(root, "chdata"), "stop is not the end of a run")

	destroy(t, ctx, runtime)
	require.True(t, processGone(server), "destroy stops the server")
	require.NoDirExists(t, root, "a disposable run must not leave its state directory behind")
}

// TestNixPersistentRunKeepsItsData runs a real server (it needs nix and the
// clickhouse binary): a plain run's rows are there after Destroy and for the
// next run of the same service.
func TestNixPersistentRunKeepsItsData(t *testing.T) {
	requireNix(t)
	useTestStore(t)
	ctx := context.Background()
	fixture := newNativeFixture(t, ctx)

	first := fixture.load(t, ctx, "", false)
	t.Cleanup(func() {
		if first.nixRuntime != nil {
			_ = first.nixRuntime.Stop(ctx)
		}
	})
	fixture.initNative(t, ctx, first)
	root := first.nixRuntime.root
	server := serverOf(t, root)
	db, err := sql.Open("clickhouse", first.connection)
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE kept (id UInt64) ENGINE = MergeTree ORDER BY id")
	require.NoError(t, err)
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec("INSERT INTO kept (id) VALUES (?)", uint64(7))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, db.Close())

	destroy(t, ctx, first)
	require.True(t, processGone(server), "destroy stops the server")
	require.DirExists(t, filepath.Join(root, "chdata"), "a persistent run's data must survive destroy")

	second := fixture.load(t, ctx, "", false)
	t.Cleanup(func() {
		if second.nixRuntime != nil {
			_ = second.nixRuntime.Stop(ctx)
		}
	})
	fixture.initNative(t, ctx, second)
	require.Equal(t, root, second.nixRuntime.root)
	db, err = sql.Open("clickhouse", second.connection)
	require.NoError(t, err)
	var id uint64
	require.NoError(t, db.QueryRow("SELECT id FROM kept").Scan(&id))
	require.Equal(t, uint64(7), id)
	require.NoError(t, db.Close())

	destroy(t, ctx, second)
	require.DirExists(t, filepath.Join(root, "chdata"))
}
