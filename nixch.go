package main

// nixch.go — Docker-free clickhouse runtime (mirrors postgres' nixpg.go).
//
// The clickhouse service agent runs the server in a container by default. On
// hosts without Docker, the same agent runs clickhouse NATIVELY from a
// nix-provisioned binary: the codefly NixEnvironment materializes `clickhouse`
// from the embedded flake, and this file drives the native lifecycle the Docker
// image's entrypoint would otherwise handle — generate a minimal server config +
// users file, launch `clickhouse server`, wait for readiness, and create the
// configured database.
//
// Runtime state (data dir, config, nix cache, flake) lives ENTIRELY OUTSIDE the
// user's source tree, keyed by a hash of the service location, so a restart of
// the same service reuses the same cluster (data persists like a Docker volume)
// without ever touching the source tree (which would break flake `path:` inputs
// of dependent services).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
)

//go:embed nix/flake.nix
var chFlakeNix string

//go:embed nix/flake.lock
var chFlakeLock string

// nixClickhouse runs a native clickhouse server off a nix-provisioned binary.
type nixClickhouse struct {
	env        *runners.NixEnvironment
	flakeDir   string
	dataDir    string
	configPath string
	usersPath  string
	port       uint16
	user       string
	password   string
	dbName     string
	logLevel   string
	out        io.Writer
	proc       runners.Proc
	// serverCtx outlives Init — starting clickhouse under the Init RPC ctx would
	// kill it the instant Init returns. Cancelled only by Stop.
	serverCtx    context.Context
	serverCancel context.CancelFunc
	// binDir is the absolute nix store bin dir holding the `clickhouse` binary.
	binDir string
}

func serviceHash(baseDir string) string {
	sum := sha256.Sum256([]byte(baseDir))
	return hex.EncodeToString(sum[:])
}

// nixRuntimeRoot is the stable, out-of-source runtime root for a clickhouse
// service, keyed by a hash of its agent location so restarts reuse the cluster.
func nixRuntimeRoot(baseDir string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "codefly", "clickhouse", serviceHash(baseDir)[:16]), nil
}

func newNixClickhouse(ctx context.Context, baseDir string, port uint16, user, password, dbName, logLevel string, out io.Writer) (*nixClickhouse, error) {
	runtimeRoot, err := nixRuntimeRoot(baseDir)
	if err != nil {
		return nil, err
	}
	flakeDir := filepath.Join(runtimeRoot, "nix")
	if err := os.MkdirAll(flakeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create nix flake dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.nix"), []byte(chFlakeNix), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.lock"), []byte(chFlakeLock), 0o644); err != nil {
		return nil, fmt.Errorf("write flake.lock: %w", err)
	}
	env, err := runners.NewNixEnvironment(ctx, flakeDir)
	if err != nil {
		return nil, fmt.Errorf("nix environment (is nix installed?): %w", err)
	}
	env.WithCacheDir(filepath.Join(runtimeRoot, ".nix-cache"))

	return &nixClickhouse{
		env:        env,
		flakeDir:   flakeDir,
		dataDir:    filepath.Join(runtimeRoot, "chdata"),
		configPath: filepath.Join(runtimeRoot, "config.xml"),
		usersPath:  filepath.Join(runtimeRoot, "users.xml"),
		port:       port,
		user:       user,
		password:   password,
		dbName:     dbName,
		logLevel:   logLevel,
		out:        out,
	}, nil
}

// Init materializes the nix env, writes the config, launches the server, waits
// for readiness, and creates the configured database.
func (n *nixClickhouse) Init(ctx context.Context) error {
	if err := n.env.Init(ctx); err != nil {
		return fmt.Errorf("materialize nix clickhouse env: %w", err)
	}
	if err := n.resolveBinDir(); err != nil {
		return err
	}
	if err := n.writeConfig(); err != nil {
		return err
	}
	if err := n.startServer(ctx); err != nil {
		return err
	}
	if err := n.waitReady(ctx); err != nil {
		return err
	}
	return n.ensureDatabase(ctx)
}

// resolveBinDir locates the nix store bin dir holding the `clickhouse` binary.
func (n *nixClickhouse) resolveBinDir() error {
	matches, err := filepath.Glob("/nix/store/*clickhouse*/bin/clickhouse")
	if err != nil {
		return fmt.Errorf("glob nix clickhouse: %w", err)
	}
	for _, m := range matches {
		binDir := filepath.Dir(m)
		if _, err := os.Stat(filepath.Join(binDir, "clickhouse")); err == nil {
			n.binDir = binDir
			return nil
		}
	}
	return fmt.Errorf("no nix clickhouse with bin/clickhouse found in /nix/store (materialization may have failed)")
}

// writeConfig emits a minimal clickhouse-server config + users file rooted at
// the out-of-tree data dir. clickhouse refuses to start without a users config
// declaring the connecting user, so both files are always written.
func (n *nixClickhouse) writeConfig() error {
	for _, d := range []string{n.dataDir, filepath.Join(n.dataDir, "tmp"), filepath.Join(n.dataDir, "user_files")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create clickhouse dir %s: %w", d, err)
		}
	}

	level := n.logLevel
	if level == "" {
		level = "warning"
	}

	config := fmt.Sprintf(`<clickhouse>
    <logger>
        <level>%s</level>
        <console>true</console>
    </logger>
    <tcp_port>%d</tcp_port>
    <listen_host>127.0.0.1</listen_host>
    <path>%s/</path>
    <tmp_path>%s/tmp/</tmp_path>
    <user_files_path>%s/user_files/</user_files_path>
    <mark_cache_size>5368709120</mark_cache_size>
    <users_config>%s</users_config>
    <default_profile>default</default_profile>
    <default_database>default</default_database>
</clickhouse>
`, xmlEscape(level), n.port, n.dataDir, n.dataDir, n.dataDir, n.usersPath)
	if err := os.WriteFile(n.configPath, []byte(config), 0o644); err != nil {
		return fmt.Errorf("write clickhouse config: %w", err)
	}

	passwordTag := "<no_password/>"
	if n.password != "" {
		passwordTag = "<password>" + xmlEscape(n.password) + "</password>"
	}
	users := fmt.Sprintf(`<clickhouse>
    <profiles>
        <default/>
    </profiles>
    <users>
        <%s>
            %s
            <networks><ip>127.0.0.1</ip></networks>
            <profile>default</profile>
            <quota>default</quota>
            <access_management>1</access_management>
        </%s>
    </users>
    <quotas>
        <default/>
    </quotas>
</clickhouse>
`, xmlTag(n.user), passwordTag, xmlTag(n.user))
	if err := os.WriteFile(n.usersPath, []byte(users), 0o644); err != nil {
		return fmt.Errorf("write clickhouse users: %w", err)
	}
	return nil
}

func (n *nixClickhouse) startServer(ctx context.Context) error {
	proc, err := n.env.NewProcess(filepath.Join(n.binDir, "clickhouse"),
		"server", "--config-file="+n.configPath)
	if err != nil {
		return err
	}
	if n.out != nil {
		proc.WithOutput(n.out)
	}
	n.serverCtx, n.serverCancel = context.WithCancel(context.Background())
	if err := proc.Start(n.serverCtx); err != nil {
		n.serverCancel()
		return fmt.Errorf("start clickhouse: %w", err)
	}
	n.proc = proc
	return nil
}

// adminDSN connects to the always-present `default` database so we can create
// the configured database (which does not exist on first boot).
func (n *nixClickhouse) adminDSN() string {
	return fmt.Sprintf("tcp://127.0.0.1:%d?username=%s&password=%s&database=default",
		n.port, n.user, n.password)
}

func (n *nixClickhouse) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		db, err := sql.Open("clickhouse", n.adminDSN())
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			lastErr = db.PingContext(pingCtx)
			cancel()
			_ = db.Close()
			if lastErr == nil {
				return nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("clickhouse did not become ready: %w", lastErr)
}

func (n *nixClickhouse) ensureDatabase(ctx context.Context) error {
	db, err := sql.Open("clickhouse", n.adminDSN())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	// Database identifiers can't be parameterized; the name comes from settings.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", chQuoteIdent(n.dbName))); err != nil {
		return fmt.Errorf("create database %q: %w", n.dbName, err)
	}
	return nil
}

func (n *nixClickhouse) Stop(ctx context.Context) error {
	if n.serverCancel != nil {
		n.serverCancel()
	}
	if n.proc == nil {
		return nil
	}
	return n.proc.Stop(ctx)
}

// chQuoteIdent backtick-quotes a clickhouse identifier.
func chQuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// xmlTag sanitizes a user-provided string for use as an XML element name (the
// clickhouse users file nests the username as a tag). Falls back to "default".
func xmlTag(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// xmlEscape escapes XML text content.
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
