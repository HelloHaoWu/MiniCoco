//go:build realdb

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisCoordinator struct {
	client *redis.Client
	prefix string
}

var _ Coordinator = (*RedisCoordinator)(nil)

func NewRedisCoordinator(ctx context.Context, address, prefix string) (*RedisCoordinator, error) {
	if prefix == "" || len(prefix) > 128 {
		return nil, ErrStoreInvalid
	}
	config, err := redis.ParseURL(address)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL")
	}
	config.DialTimeout = 3 * time.Second
	config.ReadTimeout = 3 * time.Second
	config.WriteTimeout = 3 * time.Second
	client := redis.NewClient(config)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("cannot connect to Redis; check REDIS_URL and service availability")
	}
	return &RedisCoordinator{client: client, prefix: prefix}, nil
}
func (coord *RedisCoordinator) Close() error { return coord.client.Close() }

var releaseRedisLease = redis.NewScript(`if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) else return 0 end`)

func (coord *RedisCoordinator) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if key == "" || ttl <= 0 {
		return nil, false, ErrStoreInvalid
	}
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	token, err := storeID("lease_")
	if err != nil {
		return nil, false, err
	}
	redisKey := coord.prefix + "lock:" + key
	ok, err := coord.client.SetNX(ctx, redisKey, token, ttl).Result()
	if err != nil || !ok {
		return nil, ok, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			releaseRedisLease.Run(cleanup, coord.client, []string{redisKey}, token)
		})
	}, true, nil
}

var allowRedisWindow = redis.NewScript(`
local limit=redis.call('HGET',KEYS[1],'limit')
local per=redis.call('HGET',KEYS[1],'per')
if (not limit) or limit~=ARGV[1] or per~=ARGV[2] then
  redis.call('HSET',KEYS[1],'count',0,'limit',ARGV[1],'per',ARGV[2])
  redis.call('PEXPIRE',KEYS[1],ARGV[2])
end
local count=tonumber(redis.call('HGET',KEYS[1],'count'))
if count>=tonumber(ARGV[1]) then return 0 end
redis.call('HINCRBY',KEYS[1],'count',1)
return 1`)

func (coord *RedisCoordinator) Allow(ctx context.Context, key string, rate int, per time.Duration) bool {
	if ctx.Err() != nil || key == "" || rate <= 0 || per <= 0 {
		return false
	}
	ms := per.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	allowed, err := allowRedisWindow.Run(ctx, coord.client, []string{coord.prefix + "rate:" + key}, strconv.Itoa(rate), strconv.FormatInt(ms, 10)).Int()
	return err == nil && allowed == 1
}
func (coord *RedisCoordinator) Flag(ctx context.Context, key string) (string, bool) {
	if ctx.Err() != nil || key == "" {
		return "", false
	}
	value, err := coord.client.Get(ctx, coord.prefix+"feature_toggle:"+key).Result()
	return value, err == nil
}

type inboxEnvelope struct {
	EmitterID string `json:"emitterId"`
	Chunk     []byte `json:"chunk"`
}

func (coord *RedisCoordinator) Publish(ctx context.Context, emitterID string, chunk []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if emitterID == "" || len(emitterID) > 256 || !validInboxChunk(chunk) {
		return ErrStoreInvalid
	}
	data, err := json.Marshal(inboxEnvelope{EmitterID: emitterID, Chunk: chunk})
	if err != nil {
		return err
	}
	return coord.client.Publish(ctx, coord.prefix+"inbox", data).Err()
}

// Subscription acknowledgement is awaited before returning; Publish is still
// Redis Pub/Sub (at-most-once), not a durable queue or a commit acknowledgement.
func (coord *RedisCoordinator) SubscribeInbox(ctx context.Context, deliver func(string, []byte)) (func(), error) {
	subscription := coord.client.Subscribe(ctx, coord.prefix+"inbox")
	handshake, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := subscription.Receive(handshake)
	cancel()
	if err != nil {
		subscription.Close()
		return nil, err
	}
	lifetime, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	messages := subscription.Channel(redis.WithChannelSize(64))
	go func() {
		defer close(done)
		for {
			select {
			case <-lifetime.Done():
				return
			case message, ok := <-messages:
				if !ok {
					return
				}
				if len(message.Payload) > 2*1024*1024 {
					continue
				}
				var envelope inboxEnvelope
				if json.Unmarshal([]byte(message.Payload), &envelope) != nil || envelope.EmitterID == "" || len(envelope.EmitterID) > 256 || !validInboxChunk(envelope.Chunk) {
					continue
				}
				deliver(envelope.EmitterID, envelope.Chunk)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { stop(); subscription.Close(); <-done }) }, nil
}
