//go:build realdb

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func wireRealStores(ctx context.Context, dir string) (*sessionService, func(), error) {
	databaseName := getenv("MONGO_DATABASE", "minicoco")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(databaseName) {
		return nil, nil, fmt.Errorf("invalid MONGO_DATABASE")
	}
	meta, err := NewSQLiteMetaStore(ctx, filepath.Join(dir, "meta.sqlite"))
	if err != nil {
		return nil, nil, err
	}
	client, err := mongo.Connect(options.Client().ApplyURI(getenv("MONGO_URI", "mongodb://127.0.0.1:27017")).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		meta.Close()
		return nil, nil, fmt.Errorf("invalid MONGO_URI")
	}
	closeMongo := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.Disconnect(cleanup)
	}
	if err := client.Ping(ctx, nil); err != nil {
		closeMongo()
		meta.Close()
		return nil, nil, fmt.Errorf("cannot connect to MongoDB; check MONGO_URI and service availability")
	}
	transcript, err := NewMongoTranscriptStore(ctx, client.Database(databaseName))
	if err != nil {
		closeMongo()
		meta.Close()
		return nil, nil, fmt.Errorf("cannot initialize MongoDB store indexes")
	}
	coord, err := NewRedisCoordinator(ctx, getenv("REDIS_URL", "redis://127.0.0.1:6379/0"), getenv("REDIS_PREFIX", "minicoco:"))
	if err != nil {
		closeMongo()
		meta.Close()
		return nil, nil, err
	}
	// Do not bind the subscription to the short startup timeout context.
	lifetime, cancel := context.WithCancel(context.Background())
	stopInbox, err := coord.SubscribeInbox(lifetime, activeEmitters.dispatch)
	if err != nil {
		cancel()
		coord.Close()
		closeMongo()
		meta.Close()
		return nil, nil, fmt.Errorf("cannot subscribe to Redis inbox")
	}
	cleanup := func() { cancel(); stopInbox(); coord.Close(); closeMongo(); meta.Close() }
	return &sessionService{meta: meta, transcript: transcript, coord: coord}, cleanup, nil
}
