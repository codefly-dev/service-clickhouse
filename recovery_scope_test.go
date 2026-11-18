package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/runners/dockerrun"
)

// Source validation launches this suite below the Go runner. Fixture
// containers belong to this test process, not its inherited CLI scope.
//
// They are also disposable: every run names its service after the clock, so a
// container is never reused. A run that is killed or times out skips its
// teardown, and without the ephemeral label Codefly keeps a running container
// whose owner died, as data a later run will want back. Marking them, under a
// scope directory that outlives the run, lets the next run remove them.
func TestMain(m *testing.M) {
	code := func() int {
		scope, err := testRecoveryScope()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		dockerrun.SetEphemeralContainers(true)
		if err := dockerrun.ReapDisposableContainers(context.Background(), scope); err != nil {
			// Not this run's failure: the containers stay labelled for the next one.
			fmt.Fprintln(os.Stderr, "cannot remove containers of earlier killed test runs:", err)
		}
		return m.Run()
	}()
	os.Exit(code)
}

// testRecoveryScope is the same for every run of this suite on this host, which
// is what lets one run recognise the containers another left behind.
func testRecoveryScope() (dockerrun.ContainerRecoveryScope, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return dockerrun.ContainerRecoveryScope{}, err
	}
	dir := filepath.Join(cache, "codefly", "service-clickhouse-tests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dockerrun.ContainerRecoveryScope{}, err
	}
	return dockerrun.NewContainerRecoveryScope(dir, dir, "service-clickhouse-tests")
}
