//go:build !realdb

package main

import (
	"context"
	"fmt"
)

func wireRealStores(context.Context, string) (*sessionService, func(), error) {
	return nil, nil, fmt.Errorf("STORE_BACKEND=real requires the realdb profile: go run -modfile=go.realdb.mod -tags realdb .")
}
