package main

import (
	"context"
	"fmt"
	"strings"
)

func wireStores(ctx context.Context, dir string) (*sessionService, func(), error) {
	switch strings.ToLower(getenv("STORE_BACKEND", "file")) {
	case "file":
		service, err := newSessionService(dir)
		return service, func() {}, err
	case "real":
		return wireRealStores(ctx, dir)
	default:
		return nil, nil, fmt.Errorf("STORE_BACKEND must be file or real")
	}
}
