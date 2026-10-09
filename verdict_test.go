package main

import (
	"context"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
)

// The CLI's one rule for a test run (cli pkg/testrun): nothing but an explicit
// Result.State == PASSED is success -- not the deprecated status field, not an
// empty failure list, not the legacy counters. A response that carries only
// those decodes as UNKNOWN and the conformance gate refuses the agent. This
// pins the verdict the agent reports, so a core helper that stops filling
// Result cannot silently turn the release gate red again.
func TestTestReportsAnExplicitPassedVerdict(t *testing.T) {
	response, err := NewRuntime().Test(context.Background(), &runtimev0.TestRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.TestRunResult_PASSED, response.GetResult().GetState(),
		"the run-level outcome must be an explicit PASSED, the only value the CLI treats as success")
	require.Equal(t, runtimev0.TestStatus_SUCCESS, response.GetStatus().GetState())
	require.NotNil(t, response.GetCounts(), "zero cases is a stated count, not an absent one")
	require.Zero(t, response.GetCounts().GetFailed())
	require.Zero(t, response.GetCounts().GetErrored())
}
