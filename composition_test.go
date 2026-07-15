package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNewService_EmbedsBase locks the struct contract: Service must carry a
// non-nil Base (for Wool/Logger/Location/Identity promotion) and a non-nil
// Settings pointer. If either breaks, every Runtime RPC panics.
func TestNewService_EmbedsBase(t *testing.T) {
	svc := NewService()
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	if svc.Base == nil {
		t.Fatal("Service.Base is nil — services.Base embedding broken")
	}
	if svc.Settings == nil {
		t.Fatal("Service.Settings is nil")
	}
}

func TestSettings_YAMLRoundTrip(t *testing.T) {
	src := []byte(`
database-name: events
hot-reload: true
no-migration: true
log-level: warning
docker-image: clickhouse/clickhouse-server:24.8
migration-sources:
  - name: metrics
  - name: traces
    path: ../traces/db/migrations
`)
	var s Settings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if s.DatabaseName != "events" {
		t.Errorf("DatabaseName: got %q", s.DatabaseName)
	}
	if !s.HotReload || !s.NoMigration {
		t.Error("HotReload/NoMigration not populated")
	}
	if s.Image != "clickhouse/clickhouse-server:24.8" {
		t.Errorf("Image: got %q", s.Image)
	}
	if len(s.MigrationSources) != 2 || s.MigrationSources[1].Path != "../traces/db/migrations" {
		t.Errorf("MigrationSources: %+v", s.MigrationSources)
	}
}

func TestValidSourceName(t *testing.T) {
	for _, ok := range []string{"api", "metrics", "svc_1"} {
		if !validSourceName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a-b", "a;b"} {
		if validSourceName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// TestXMLHelpers guards the native-runtime config generation against injection
// through user/password/log-level values.
func TestXMLHelpers(t *testing.T) {
	if got := xmlTag("def-ault user!"); got != "defaultuser" {
		t.Errorf("xmlTag stripped wrong: %q", got)
	}
	if got := xmlTag("!!!"); got != "default" {
		t.Errorf("xmlTag fallback: %q", got)
	}
	if got := xmlEscape(`a<b>&"'`); got != "a&lt;b&gt;&amp;&quot;&apos;" {
		t.Errorf("xmlEscape: %q", got)
	}
	if got := chQuoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("chQuoteIdent: %q", got)
	}
}

func TestMigrationSources_Resolution(t *testing.T) {
	root := t.TempDir()
	svcDir := filepath.Join(root, "store")
	mustMkdir(t, filepath.Join(svcDir, "migrations"))
	mustMkdir(t, filepath.Join(root, "metrics", "migrations"))

	s := NewRuntime()
	s.Location = svcDir
	s.Settings.DatabaseName = "events"
	s.Settings.MigrationSources = []MigrationSource{
		{Name: "metrics"},
		{Name: "ghost"}, // missing dir → skipped
	}

	sources, err := s.migrationSources(context.Background())
	if err != nil {
		t.Fatalf("migrationSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("expected own + metrics, got %d: %+v", len(sources), sources)
	}
	if sources[0].table != "" {
		t.Errorf("own source should use default table, got %q", sources[0].table)
	}
	if sources[1].name != "metrics" || sources[1].table != "schema_migrations_metrics" {
		t.Errorf("metrics source: %+v", sources[1])
	}
}

func TestClickHouseTableName(t *testing.T) {
	got, err := clickHouseTableName("orders-api")
	if err != nil {
		t.Fatal(err)
	}
	if got != "orders_api" {
		t.Fatalf("table name = %q, want orders_api", got)
	}
	for _, invalid := range []string{"", "9orders", "orders; DROP TABLE users"} {
		if _, err := clickHouseTableName(invalid); err == nil {
			t.Errorf("clickHouseTableName(%q) accepted an invalid identifier", invalid)
		}
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}
