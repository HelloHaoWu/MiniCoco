//go:build !realdb

package main

import (
	"context"
	"strings"
	"testing"
)

func TestWireRealUnavailableInDefaultBuild(t *testing.T) {
	t.Setenv("STORE_BACKEND", "real")
	if _, _, err := wireStores(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "-modfile=go.realdb.mod -tags realdb") {
		t.Fatal("default binary must explain how to enable the optional profile")
	}
}
