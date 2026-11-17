package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/runners/dockerrun"
)

// Source validation launches this suite below the Go runner. Fixture
// containers belong to this test process, not its inherited CLI scope.
// machineNativeStore is where the agent keeps native state outside this suite.
var machineNativeStore = nativeStore

func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "service-clickhouse-test-scope-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		scope, err := dockerrun.NewContainerRecoveryScope(dir, dir, "service-clickhouse-tests")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		// Native state of this suite lives under the suite's own directory,
		// never beside a developer's real runs: an agent that initializes
		// stops what killed runs left in its store.
		nativeStore = func() string { return filepath.Join(dir, "codefly", "clickhouse") }
		return m.Run()
	}()
	os.Exit(code)
}
