package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Two stacks of one service under different naming scopes run side by side;
// a shared native data dir makes the second server fail to lock it (#47).
func TestNixRuntimeRootIsKeyedByNamingScope(t *testing.T) {
	const location = "/workspace/modules/tracing/services/evidence"

	unscoped, err := nixRuntimeRoot(location, "")
	if err != nil {
		t.Fatal(err)
	}
	review, err := nixRuntimeRoot(location, "review")
	if err != nil {
		t.Fatal(err)
	}
	parity, err := nixRuntimeRoot(location, "parity3")
	if err != nil {
		t.Fatal(err)
	}
	if review == parity || review == unscoped || parity == unscoped {
		t.Fatalf("naming scopes share a runtime root: unscoped=%s review=%s parity3=%s", unscoped, review, parity)
	}

	again, err := nixRuntimeRoot(location, "review")
	if err != nil {
		t.Fatal(err)
	}
	if again != review {
		t.Fatalf("runtime root is not stable across restarts: %s then %s", review, again)
	}
}

// An unscoped run keeps the key it had before scopes were part of it, so its
// existing data dir survives the upgrade.
func TestUnscopedServiceHashIsUnchanged(t *testing.T) {
	const location = "/workspace/modules/tracing/services/evidence"
	sum := sha256.Sum256([]byte(location))
	if got, want := serviceHash(location, ""), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("unscoped hash changed: got %s, want %s", got, want)
	}
}
