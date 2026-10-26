//go:build realdb

package main

import (
	"context"
	"encoding/json"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (store *MongoTranscriptStore) Summaries(ctx context.Context, sid string) ([]Summary, error) {
	if !validSessionID(sid) {
		return nil, ErrStoreInvalid
	}
	cursor, err := store.summaries.Find(ctx, bson.M{"sessionId": sid}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	result := []Summary{}
	for cursor.Next(ctx) {
		var row struct {
			Data []byte `bson:"checkpointJson"`
		}
		if err := cursor.Decode(&row); err != nil {
			return nil, err
		}
		if len(row.Data) == 0 {
			continue
		}
		var s Summary
		if json.Unmarshal(row.Data, &s) != nil {
			return nil, ErrStoreInvalid
		}
		result = append(result, s)
	}
	return result, cursor.Err()
}
func (store *MongoTranscriptStore) Latest(ctx context.Context, sid string, n int) ([]Message, error) {
	raw, err := store.RawLatest(ctx, sid, allHistory)
	if err != nil {
		return nil, err
	}
	summaries, err := store.Summaries(ctx, sid)
	if err != nil {
		return nil, err
	}
	return mergedHistory(raw, summaries, n)
}
