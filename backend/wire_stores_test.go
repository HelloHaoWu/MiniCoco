package main

import (
	"context"
	"testing"
)

func TestWireFileAndInvalidBackend(t *testing.T) {
	t.Setenv("STORE_BACKEND", "file")
	t.Setenv("MONGO_URI", "invalid unused configuration")
	t.Setenv("REDIS_URL", "invalid unused configuration")
	service, closeStores, err := wireStores(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStores()
	if _, ok := service.meta.(*JSONMetaStore); !ok {
		t.Fatal("file selection did not use the default store")
	}
	t.Setenv("STORE_BACKEND", "unknown")
	if _, _, err := wireStores(context.Background(), t.TempDir()); err == nil {
		t.Fatal("invalid storage backend accepted")
	}
}
