//go:build realdb

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// One document owns a complete round. A unique (sessionId,roundId) index would
// incorrectly reject the second message if applied to individual message rows.
// An envelope gives standalone MongoDB the same atomic batch boundary as files,
// without requiring a replica-set transaction for input + pending/final.
// JSON bytes preserve OpenAI field names and arbitrary JSON integer precision.
// IDs/status/usage/order remain indexed, queryable BSON fields.
type mongoEnvelope struct {
	SessionID   string `bson:"sessionId"`
	RoundID     string `bson:"roundId"`
	MsgID       string `bson:"msgId,omitempty"`
	Kind        string `bson:"kind"`
	Status      string `bson:"status"`
	Seq         int64  `bson:"seq"`
	TS          int64  `bson:"ts"`
	Input       []byte `bson:"inputJson,omitempty"`
	Reply       []byte `bson:"replyJson,omitempty"`
	Events      []byte `bson:"eventsJson,omitempty"`
	Fingerprint string `bson:"fingerprint,omitempty"`
	BaseCount   int    `bson:"baseCount"`
	InputCount  int    `bson:"inputCount"`
	Usage       Usage  `bson:"usage"`
}

type MongoTranscriptStore struct {
	database  *mongo.Database
	messages  *mongo.Collection
	summaries *mongo.Collection
	counters  *mongo.Collection
}

var _ TranscriptStore = (*MongoTranscriptStore)(nil)

func NewMongoTranscriptStore(ctx context.Context, database *mongo.Database) (*MongoTranscriptStore, error) {
	store := &MongoTranscriptStore{database: database, messages: database.Collection("auto_llm_message"), summaries: database.Collection("auto_llm_summary"), counters: database.Collection("auto_llm_sequence")}
	_, err := store.messages.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "sessionId", Value: 1}, {Key: "roundId", Value: 1}}, Options: options.Index().SetName("unique_session_round").SetUnique(true)},
		{Keys: bson.D{{Key: "msgId", Value: 1}}, Options: options.Index().SetName("unique_reply_id").SetUnique(true).SetPartialFilterExpression(bson.M{"msgId": bson.M{"$type": "string"}})},
		{Keys: bson.D{{Key: "sessionId", Value: 1}, {Key: "seq", Value: 1}}, Options: options.Index().SetName("session_order")},
	})
	if err != nil {
		return nil, err
	}
	_, err = store.summaries.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "sessionId", Value: 1}, {Key: "ts", Value: 1}}})
	return store, err
}

func mongoStoreError(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) {
		return ErrStoreNotFound
	}
	return err
}

func (store *MongoTranscriptStore) sequence(ctx context.Context, sid string) (int64, error) {
	if !validSessionID(sid) {
		return 0, ErrStoreInvalid
	}
	for attempt := 0; attempt < 3; attempt++ {
		var result struct {
			Seq int64 `bson:"seq"`
		}
		err := store.counters.FindOneAndUpdate(ctx, bson.M{"_id": sid}, bson.M{"$inc": bson.M{"seq": int64(1)}}, options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&result)
		if mongo.IsDuplicateKeyError(err) {
			continue
		}
		return result.Seq, err
	}
	return 0, ErrStoreConflict
}

func encodeMongoMessages(messages []Message) ([]byte, error) {
	copies := make([]Message, 0, len(messages))
	for _, message := range messages {
		copy, err := cloneStoreMessage(message)
		if err != nil {
			return nil, err
		}
		copies = append(copies, copy)
	}
	return json.Marshal(copies)
}

func decodeMongoMessages(data []byte) ([]Message, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var messages []Message
	err := decoder.Decode(&messages)
	return messages, err
}

func (store *MongoTranscriptStore) envelope(ctx context.Context, sid, rid string) (mongoEnvelope, error) {
	if !validSessionID(sid) {
		return mongoEnvelope{}, ErrStoreInvalid
	}
	var document mongoEnvelope
	err := store.messages.FindOne(ctx, bson.M{"sessionId": sid, "roundId": rid}).Decode(&document)
	return document, mongoStoreError(err)
}

func (store *MongoTranscriptStore) Append(ctx context.Context, sid, rid string, messages []Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSessionID(sid) || rid == "" || len(rid) > 256 || len(messages) == 0 {
		return ErrStoreInvalid
	}
	if _, err := store.envelope(ctx, sid, rid); err == nil {
		return nil
	} else if !errors.Is(err, ErrStoreNotFound) {
		return err
	}
	input, err := encodeMongoMessages(messages)
	if err != nil {
		return err
	}
	seq, err := store.sequence(ctx, sid)
	if err != nil {
		return err
	}
	_, err = store.messages.InsertOne(ctx, mongoEnvelope{SessionID: sid, RoundID: rid, Kind: "append", Status: "final", Seq: seq, TS: time.Now().UnixMilli(), Input: input})
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

func (store *MongoTranscriptStore) PreoccupyReply(ctx context.Context, sid string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	seq, err := store.sequence(ctx, sid)
	if err != nil {
		return "", err
	}
	suffix, err := storeID("m_")
	if err != nil {
		return "", err
	}
	id := sid + "." + suffix
	_, err = store.messages.InsertOne(ctx, mongoEnvelope{SessionID: sid, RoundID: "reply-" + suffix, MsgID: id, Kind: "reply", Status: "pending", Seq: seq, TS: time.Now().UnixMilli()})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (store *MongoTranscriptStore) UpdateReply(ctx context.Context, id string, message Message, usage Usage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sid, err := sessionFromMessageID(id)
	if err != nil {
		return err
	}
	copy, err := cloneStoreMessage(message)
	if err != nil {
		return err
	}
	if copy["role"] != "assistant" || !validUsage(usage) {
		return ErrStoreInvalid
	}
	reply, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	var previous mongoEnvelope
	if err := store.messages.FindOne(ctx, bson.M{"sessionId": sid, "msgId": id}).Decode(&previous); err != nil {
		return mongoStoreError(err)
	}
	if previous.Kind != "reply" {
		return ErrStoreInvalid
	}
	if previous.Status == "final" {
		if bytes.Equal(previous.Reply, reply) && previous.Usage == usage {
			return nil
		}
		return ErrStoreConflict
	}
	result, err := store.messages.UpdateOne(ctx, bson.M{"sessionId": sid, "msgId": id, "kind": "reply", "status": "pending"}, bson.M{"$set": bson.M{"status": "final", "replyJson": reply, "usage": usage}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return store.UpdateReply(ctx, id, message, usage)
	}
	return nil
}

func (store *MongoTranscriptStore) documents(ctx context.Context, sid string) ([]mongoEnvelope, error) {
	if !validSessionID(sid) {
		return nil, ErrStoreInvalid
	}
	cursor, err := store.messages.Find(ctx, bson.M{"sessionId": sid}, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var result []mongoEnvelope
	err = cursor.All(ctx, &result)
	return result, err
}

func (store *MongoTranscriptStore) Latest(ctx context.Context, sid string, n int) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, ErrStoreInvalid
	}
	documents, err := store.documents(ctx, sid)
	if err != nil {
		return nil, err
	}
	result := []Message{}
	for _, document := range documents {
		if document.Status != "final" {
			continue
		}
		if len(document.Input) > 0 {
			input, err := decodeMongoMessages(document.Input)
			if err != nil {
				return nil, err
			}
			result = append(result, input...)
		}
		if len(document.Reply) > 0 {
			var reply Message
			decoder := json.NewDecoder(bytes.NewReader(document.Reply))
			decoder.UseNumber()
			if err := decoder.Decode(&reply); err != nil {
				return nil, err
			}
			result = append(result, reply)
		}
	}
	if n < len(result) {
		result = result[len(result)-n:]
	}
	return result, nil
}

func (store *MongoTranscriptStore) SaveSummary(ctx context.Context, sid string, summary Summary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSessionID(sid) || summary.OriginStartID == "" || summary.OriginEndID == "" || strings.TrimSpace(summary.Content) == "" {
		return ErrStoreInvalid
	}
	_, err := store.summaries.InsertOne(ctx, bson.M{"sessionId": sid, "originStartId": summary.OriginStartID, "originEndId": summary.OriginEndID, "content": summary.Content, "ts": time.Now().UnixMilli()})
	return err
}

func envelopeRound(document mongoEnvelope) (StoredRound, error) {
	if document.Kind != "round" {
		return StoredRound{}, ErrStoreNotFound
	}
	var events []Event
	if len(document.Events) > 0 {
		if err := json.Unmarshal(document.Events, &events); err != nil {
			return StoredRound{}, err
		}
	}
	return StoredRound{ID: document.RoundID, ReplyID: document.MsgID, Fingerprint: document.Fingerprint, BaseCount: document.BaseCount, InputCount: document.InputCount, Final: document.Status == "final", Usage: document.Usage, Events: events}, nil
}

func (store *MongoTranscriptStore) Round(ctx context.Context, sid, rid string) (StoredRound, error) {
	document, err := store.envelope(ctx, sid, rid)
	if err != nil {
		return StoredRound{}, err
	}
	return envelopeRound(document)
}

func (store *MongoTranscriptStore) ReserveRound(ctx context.Context, sid, rid, fingerprint string, input []Message, baseCount int) (StoredRound, error) {
	if err := ctx.Err(); err != nil {
		return StoredRound{}, err
	}
	if !validSessionID(sid) || rid == "" || len(rid) > 256 || len(fingerprint) != 64 || len(input) == 0 || baseCount < 0 {
		return StoredRound{}, ErrStoreInvalid
	}
	if previous, err := store.Round(ctx, sid, rid); err == nil {
		if previous.Fingerprint != fingerprint {
			return StoredRound{}, ErrStoreConflict
		}
		return previous, nil
	} else if !errors.Is(err, ErrStoreNotFound) {
		return StoredRound{}, err
	}
	encoded, err := encodeMongoMessages(input)
	if err != nil {
		return StoredRound{}, err
	}
	seq, err := store.sequence(ctx, sid)
	if err != nil {
		return StoredRound{}, err
	}
	suffix, err := storeID("m_")
	if err != nil {
		return StoredRound{}, err
	}
	document := mongoEnvelope{SessionID: sid, RoundID: rid, MsgID: sid + "." + suffix, Kind: "round", Status: "pending", Seq: seq, TS: time.Now().UnixMilli(), Input: encoded, Fingerprint: fingerprint, BaseCount: baseCount, InputCount: len(input)}
	if _, err := store.messages.InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			previous, err := store.Round(ctx, sid, rid)
			if err != nil {
				return StoredRound{}, err
			}
			if previous.Fingerprint != fingerprint {
				return StoredRound{}, ErrStoreConflict
			}
			return previous, nil
		}
		return StoredRound{}, err
	}
	return envelopeRound(document)
}

func (store *MongoTranscriptStore) CommitRound(ctx context.Context, sid, rid string, reply Message, usage Usage, events []Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSessionID(sid) || !validUsage(usage) || len(events) == 0 || events[len(events)-1].Type != EvDone {
		return ErrStoreInvalid
	}
	copy, err := cloneStoreMessage(reply)
	if err != nil {
		return err
	}
	if copy["role"] != "assistant" {
		return ErrStoreInvalid
	}
	replyJSON, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	eventJSON, err := json.Marshal(events)
	if err != nil {
		return err
	}
	result, err := store.messages.UpdateOne(ctx, bson.M{"sessionId": sid, "roundId": rid, "kind": "round", "status": "pending"}, bson.M{"$set": bson.M{"status": "final", "replyJson": replyJSON, "usage": usage, "eventsJson": eventJSON}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		if _, err := store.Round(ctx, sid, rid); err != nil {
			return err
		}
		return ErrStoreConflict
	}
	return nil
}

func (store *MongoTranscriptStore) CommittedRounds(ctx context.Context, sid string) ([]StoredRound, error) {
	documents, err := store.documents(ctx, sid)
	if err != nil {
		return nil, err
	}
	result := []StoredRound{}
	for _, document := range documents {
		if document.Kind != "round" || document.Status != "final" {
			continue
		}
		round, err := envelopeRound(document)
		if err != nil {
			return nil, err
		}
		result = append(result, round)
	}
	return result, nil
}
