package main

import (
	"database/sql"
	"os"
	"testing"
)

func TestZRepro(t *testing.T) {
	dsn := os.Getenv("CODEFLY_CLICKHOUSE_REPRO_DSN")
	if dsn == "" {
		t.Skip("set CODEFLY_CLICKHOUSE_REPRO_DSN to run the manual ClickHouse parser reproduction")
	}
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"multiline_paren":     "CREATE TABLE IF NOT EXISTS r1 (\n    id UUID,\n    created_at DateTime DEFAULT now()\n) ENGINE = MergeTree()\nORDER BY (id)",
		"singleline":          "CREATE TABLE IF NOT EXISTS r2 (id UUID) ENGINE = MergeTree() ORDER BY id",
		"multiline_noparenfn": "CREATE TABLE IF NOT EXISTS r3 (\n    id UUID,\n    created_at DateTime DEFAULT now()\n) ENGINE = MergeTree ORDER BY (id)",
	}
	for name, q := range cases {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("[%s] FAILED: %v", name, err)
		} else {
			t.Logf("[%s] OK", name)
		}
	}
}
