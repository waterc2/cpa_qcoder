package main

import (
	"os"
	"testing"

	"qoder2api/account"
)

// TestMain isolates plugin tests from the operator's real data directory.
//
// The plugin reads <data-dir>/models.json for its model denylist. Without this,
// tests that run after TestRegistration (which triggers initDataDir) would read
// the real ~/.qoder2api-cpa/models.json and, if it disables every static model,
// TestModelStatic would fail. It also keeps the tests from ever writing to the
// operator's real data directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "qoder-plugin-test-*")
	if err != nil {
		os.Exit(1)
	}
	_ = os.Setenv("QODER2API_HOME", dir)
	// Point the account package at the same throwaway directory up front so
	// tests that never call initDataDir still stay isolated.
	account.SetDataRoot(dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
