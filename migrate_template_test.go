package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// renderedMigrateScript renders the migration image recipe the way the CLI asks
// for it and returns the path of the migrate.sh it carries.
func renderedMigrateScript(t *testing.T) string {
	t.Helper()
	builder := loadedBuilder(t)
	output := t.TempDir()
	response, err := builder.Build(context.Background(), dockerBuildRequest(output))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, response.GetState().GetState(), response.GetState().GetMessage())
	script := filepath.Join(output, "builder", "migrate.sh")
	require.FileExists(t, script)
	return script
}

func requireShell(t *testing.T) string {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX sh on PATH: the migrate.sh encoder cannot be exercised here")
	}
	return shell
}

// encoderInputs are the credentials connection_template_test.go holds the
// template to, plus the bytes a query-string decoder treats specially.
var encoderInputs = []string{
	"user", "password",
	"evidence", "a1b2c3d4e5f6",
	"ev-id_ence.1", "p@ss:w/rd?&+ 1",
	"us@er", "%25already#encoded",
	"ünïcode", "pässwörd",
	"+", "~", "%", " ", "a=b&c=d", "tab\there", "line\nbreak", "quote\"'`$(x)\\",
	"tracing_evidence",
}

// migrate.sh percent-encodes the credentials and database it puts in the DSN's
// query string. clickhouse-go v1 decodes them with the query decoder, where
// "+" is a space, so the encoder must leave nothing but the unreserved set
// unescaped -- the same escape, byte for byte, as core's URL_USERINFO, which
// the connection consumers read is assembled with.
func TestMigrateScriptEncoderMatchesCoreUserinfoEscape(t *testing.T) {
	shell := requireShell(t)
	script := renderedMigrateScript(t)
	for _, value := range encoderInputs {
		command := exec.Command(shell, "-c", `. "$0"; percent_encode "$1"`, script, value)
		command.Env = append(os.Environ(), "MIGRATE_SH_DEFINE_ONLY=1")
		encoded, err := command.Output()
		require.NoError(t, err, "%q", value)
		require.Equal(t, escapeUserinfo(value), string(encoded), "%q", value)
	}
}

// The same encoder under the image's own shell: alpine's busybox sh, od and
// printf, which is what the Job runs. Skipped when Docker is unavailable.
func TestMigrateScriptEncoderUnderBusybox(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping the busybox encoder run")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable: the busybox encoder run is skipped")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon unavailable: the busybox encoder run is skipped")
	}
	script := renderedMigrateScript(t)
	// One container for every input: each line of output is one encoding.
	program := `. /migrate.sh; shift; for value in "$@"; do percent_encode "$value"; echo; done`
	args := []string{"run", "--rm", "-e", "MIGRATE_SH_DEFINE_ONLY=1",
		"-v", script + ":/migrate.sh:ro", "alpine:3.21", "sh", "-c", program, "sh", "ignored"}
	args = append(args, encoderInputs...)
	output, err := exec.Command("docker", args...).Output()
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	require.Len(t, lines, len(encoderInputs))
	for i, value := range encoderInputs {
		require.Equal(t, escapeUserinfo(value), lines[i], "%q", value)
	}
}

// migrate.sh waits for the server, builds the query-form DSN clickhouse-go v1
// reads, hands it to migrate, and never prints the password -- not even when
// migrate's own error quotes the URL. Fake nc and migrate stand in for the
// network and the binary.
func TestMigrateScriptAssemblesTheQueryDSNAndRedactsThePassword(t *testing.T) {
	shell := requireShell(t)
	script := renderedMigrateScript(t)
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "nc"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "migrate"), []byte(`#!/bin/sh
printf '%s\n' "$@" > "$MIGRATE_ARGS_FILE"
echo "error: failed to open $4"
[ -z "${MIGRATE_SAY:-}" ] || echo "$MIGRATE_SAY"
exit "${MIGRATE_EXIT:-0}"
`), 0o755))

	run := func(exit string, environment ...string) (string, error) {
		command := exec.Command(shell, script)
		command.Env = append([]string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"MIGRATE_ARGS_FILE=" + argsFile,
			"MIGRATE_EXIT=" + exit,
			"CLICKHOUSE_HOST=evidence.platform-obin-tracing.svc.cluster.local",
			"CLICKHOUSE_PORT=31234",
			"CLICKHOUSE_DB=tracing_evidence",
			"CLICKHOUSE_USER=ev@dence",
		}, environment...)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		return output.String(), err
	}

	const password = "p@ss:w/rd?&+ 1.x"
	output, err := run("1", "CLICKHOUSE_PASSWORD="+password)
	require.Error(t, err, output)
	require.Contains(t, output, "failed (migrate exited 1)")
	require.Contains(t, output, "password=REDACTED")
	require.NotContains(t, output, escapeUserinfo(password))
	require.NotContains(t, output, password)
	require.Contains(t, output, "username=REDACTED", "the user is redacted too")
	require.NotContains(t, output, "ev%40dence")

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Equal(t, strings.Join([]string{
		"-path", "/app/migrations", "-database",
		"clickhouse://evidence.platform-obin-tracing.svc.cluster.local:31234?username=ev%40dence&password=" + escapeUserinfo(password) + "&database=tracing_evidence&x-multi-statement=true",
		"up",
	}, "\n")+"\n", string(args))

	// Redaction is value-independent: whatever migrate prints after username=
	// or password= is hidden, not just the value this run encoded.
	output, err = run("1", "CLICKHOUSE_PASSWORD=other", "MIGRATE_SAY=dial: tcp://h?username=u&password=p+w&database=d failed")
	require.Error(t, err, output)
	require.Contains(t, output, "username=REDACTED&password=REDACTED&database=d")
	require.NotContains(t, output, "p+w")

	output, err = run("0", "CLICKHOUSE_PASSWORD=")
	require.NoError(t, err, output)
	require.Contains(t, output, "is up to date")

	output, err = run("0")
	require.Error(t, err, output)
	require.Contains(t, output, "CLICKHOUSE_PASSWORD is not set")

	output, err = run("0", "CLICKHOUSE_PASSWORD=x", "CLICKHOUSE_READY_TIMEOUT_SECONDS=soon")
	require.Error(t, err, output)
	require.Contains(t, output, "CLICKHOUSE_READY_TIMEOUT_SECONDS must be a whole number of seconds")

	// A leading zero is decimal, not an octal arithmetic error.
	output, err = run("0", "CLICKHOUSE_PASSWORD=x", "CLICKHOUSE_READY_TIMEOUT_SECONDS=08")
	require.NoError(t, err, output)
	require.Contains(t, output, "is up to date")
}

// The wait is bounded: a server that never listens fails the Job by name
// instead of hanging it.
func TestMigrateScriptWaitIsBounded(t *testing.T) {
	shell := requireShell(t)
	script := renderedMigrateScript(t)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "nc"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	command := exec.Command(shell, script)
	command.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CLICKHOUSE_HOST=nowhere", "CLICKHOUSE_PORT=9000", "CLICKHOUSE_DB=db",
		"CLICKHOUSE_USER=u", "CLICKHOUSE_PASSWORD=p", "CLICKHOUSE_READY_TIMEOUT_SECONDS=000",
	}
	output, err := command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "did not accept connections within 0s")
}

// The recipe carries migrate.sh beside the Dockerfile, the image runs it, and
// nothing in the recipe names a connection carrier or a credential.
func TestBuildRecipeCarriesTheMigrationScript(t *testing.T) {
	script := renderedMigrateScript(t)
	dockerfile, err := os.ReadFile(filepath.Join(filepath.Dir(script), "Dockerfile"))
	require.NoError(t, err)
	require.Contains(t, string(dockerfile), "COPY builder/migrate.sh /app/migrate.sh")
	require.Contains(t, string(dockerfile), `CMD ["/bin/sh", "/app/migrate.sh"]`)
	require.Contains(t, string(dockerfile), "migrate/releases/download/v4.18.1/")
	// Only the recipe it names goes into the image: never the build context
	// wholesale, which in the in-agent build is the service directory and its
	// local secret configuration.
	require.NotContains(t, string(dockerfile), "COPY . .")
	require.NotContains(t, string(dockerfile), "COPY configurations")
	for _, file := range []string{script, filepath.Join(filepath.Dir(script), "Dockerfile")} {
		content, err := os.ReadFile(file)
		require.NoError(t, err)
		require.NotContains(t, string(content), "CODEFLY__", file)
		require.NotContains(t, string(content), "{{", file)
	}
}
