package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// The bootstrap Job printed its clickhouse password. Its entrypoint hands the
// migrate CLI a URL carrying the credentials in the query (the only place
// clickhouse-go v1 reads them), and migrate v4.18.1 -- the CLI the image
// downloaded -- reports a failed connection as
//
//	error: failed to open database, "clickhouse://host:80?username=u&password=<the password>&...": driver: bad connection
//
// so every failed run wrote the password into the cluster's logs. v4.19.1
// dropped the URL from that message, but its clickhouse driver still returns
// net/url's parse error, which quotes the whole URL, for an address that does
// not parse. These tests run the entrypoint the build renders -- the same shell,
// against the migrate CLI built from the release this module links -- and hold
// that no failure it reports carries the password.

const fakePassword = "fake-pw-7f3c91e2"

func TestBootstrapEntrypointNeverPrintsThePassword(t *testing.T) {
	ctx := context.Background()
	builder := loadedBuilder(t)
	script := renderedEntrypoint(ctx, t, builder)
	migrate := buildMigrateCLI(t)
	migrations := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(migrations, "1_init.up.sql"), []byte("SELECT 1;"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(migrations, "1_init.down.sql"), []byte("SELECT 1;"), 0o644))
	script = strings.ReplaceAll(script, "/usr/local/bin/migrate", migrate)
	script = strings.ReplaceAll(script, "/app/migrations", migrations)

	for name, run := range map[string]struct {
		address  string
		password string
	}{
		// What the obin cell logged: the server is not answering.
		"unreachable server": {address: "127.0.0.1:1", password: fakePassword},
		// The address does not parse: net/url quotes the whole URL.
		"unparsable address": {address: "bad host:9000", password: fakePassword},
		// A password that is a substring of the redaction marker must still
		// terminate and still be hidden.
		"password inside the marker": {address: "bad host:9000", password: "redacted"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			cmd.Env = append(os.Environ(),
				builder.migrationAddressKey()+"="+run.address,
				builder.migrationUserKey()+"=migrator",
				builder.migrationPasswordKey()+"="+run.password,
			)
			out, err := cmd.CombinedOutput()
			output := string(out)
			var exit *exec.ExitError
			require.True(t, errors.As(err, &exit), "the entrypoint must fail when migrate fails, got err=%v output=%q", err, output)
			require.NotEqual(t, 0, exit.ExitCode())
			require.Contains(t, output, "failed to open database", "the failure must still be reported")
			if !strings.Contains("<redacted>", run.password) {
				require.NotContains(t, output, run.password, "the entrypoint printed the password")
			}
			require.NotRegexp(t, `password=[^<&"]`, output, "the password parameter must be redacted")
		})
	}
}

// TestMigrationImageRunsTheLinkedMigrateRelease holds the image's migrate CLI
// to the release this module links: the CLI used to sit at v4.18.1 -- the one
// that prints the URL -- while the agent's own in-process migrations ran
// v4.19.1.
func TestMigrationImageRunsTheLinkedMigrateRelease(t *testing.T) {
	builder := loadedBuilder(t)
	dockerfile := renderedDockerfile(context.Background(), t, builder)
	linked := linkedVersion(t, migrateModule)
	require.Contains(t, dockerfile, "-X main.Version="+linked,
		"the migration image must build the migrate CLI at the release this module links (%s)", linked)
}

func linkedVersion(t *testing.T, module string) string {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)
	for _, dep := range info.Deps {
		if dep.Path == module {
			return dep.Version
		}
	}
	t.Fatalf("%s is not linked", module)
	return ""
}

func renderedDockerfile(ctx context.Context, t *testing.T, builder *Builder) string {
	t.Helper()
	recipe := t.TempDir()
	build, err := builder.Build(ctx, dockerBuildRequest(recipe))
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, build.GetState().GetState(), build.GetState().GetMessage())
	content, err := os.ReadFile(filepath.Join(recipe, "builder", "Dockerfile"))
	require.NoError(t, err)
	return string(content)
}

// renderedEntrypoint is the shell the image runs: the Dockerfile's shell-form
// CMD with its line continuations joined, as docker hands it to `sh -c`.
func renderedEntrypoint(ctx context.Context, t *testing.T, builder *Builder) string {
	t.Helper()
	dockerfile := renderedDockerfile(ctx, t, builder)
	_, cmd, found := strings.Cut(dockerfile, "\nCMD ")
	require.True(t, found, "the Dockerfile has no shell-form CMD")
	return regexp.MustCompile(`\\\n`).ReplaceAllString(cmd, "")
}

// buildMigrateCLI builds the migrate CLI, clickhouse driver only, at the
// release this module links -- the one the image downloads.
func buildMigrateCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "migrate")
	cmd := exec.Command("go", "build", "-tags", "clickhouse", "-o", binary, migrateModule+"/cmd/migrate")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "cannot build the migrate CLI: %s", out)
	return binary
}

// The agent's own migrations and readiness probes open a DSN with clickhouse-go
// v1, whose parse error quotes the whole DSN. nativeDSN refuses an address that
// would not parse, naming the address alone, and escapes the credentials so a
// password cannot make the DSN unparsable either.
func TestNativeDSNNeverQuotesThePassword(t *testing.T) {
	_, err := nativeDSN("bad host:9000", "migrator", fakePassword, "db")
	require.Error(t, err)
	require.NotContains(t, err.Error(), fakePassword)

	awkward := fakePassword + "#%zz&database=other"
	dsn, err := nativeDSN("127.0.0.1:1", "migrator", awkward, "db")
	require.NoError(t, err)
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, awkward, parsed.Query().Get("password"))
	require.Equal(t, "db", parsed.Query().Get("database"))

	db, err := sql.Open("clickhouse", dsn)
	require.NoError(t, err)
	defer db.Close()
	err = db.Ping()
	require.Error(t, err)
	require.NotContains(t, err.Error(), fakePassword)
}

// A Deploy refuses an in-cluster address the Job's migrate URL could not
// parse, rather than rendering a Job whose failure would quote that URL.
func TestDeployRefusesAnAddressTheMigrateURLCannotCarry(t *testing.T) {
	useSuccessfulKubectl(t)
	builder := loadedBuilder(t)
	request := deployRequest(t, builder, builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1, t.TempDir(), nil)
	for _, instance := range request.GetNetworkMappings()[0].GetInstances() {
		instance.Address = "bad host:9000"
	}
	response, err := builder.Deploy(context.Background(), request)
	message := response.GetState().GetMessage()
	if err != nil {
		message = err.Error()
	}
	require.Contains(t, message, `clickhouse address "bad host:9000" is not a host:port`)
}
