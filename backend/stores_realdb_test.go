//go:build realdb

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func realContractFactory(t *testing.T) func() contractStores {
	t.Helper()
	mongoURI, redisURL := os.Getenv("MINICOCO_TEST_MONGO_URI"), os.Getenv("MINICOCO_TEST_REDIS_URL")
	if mongoURI == "" || redisURL == "" {
		t.Skip("real service tests require MINICOCO_TEST_MONGO_URI and MINICOCO_TEST_REDIS_URL; check_realdb.sh requires both")
	}
	token, err := storeID("test_minicoco_")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI).SetServerSelectionTimeout(3 * time.Second))
	if err != nil {
		t.Fatal("test Mongo URI is invalid")
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.Background())
		t.Fatal("test MongoDB is unavailable")
	}
	database := client.Database(token)
	prefix := token + ":"
	path := filepath.Join(t.TempDir(), "meta.sqlite")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		database.Drop(cleanup)
		client.Disconnect(cleanup)
		coord, err := NewRedisCoordinator(cleanup, redisURL, prefix)
		if err == nil {
			defer coord.Close()
			iterator := coord.client.Scan(cleanup, 0, prefix+"*", 100).Iterator()
			for iterator.Next(cleanup) {
				coord.client.Del(cleanup, iterator.Val())
			}
		}
	})
	return func() contractStores {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		meta, err := NewSQLiteMetaStore(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		transcript, err := NewMongoTranscriptStore(ctx, database)
		if err != nil {
			meta.Close()
			t.Fatal(err)
		}
		coord, err := NewRedisCoordinator(ctx, redisURL, prefix)
		if err != nil {
			meta.Close()
			t.Fatal(err)
		}
		return contractStores{meta, transcript, coord, func() { meta.Close(); coord.Close() }}
	}
}
func TestRealStoreContract(t *testing.T) { runStoreContract(t, realContractFactory) }

func TestRealSQLiteSharedLedger(t *testing.T) {
	open := realContractFactory(t)
	one, two := open(), open()
	defer one.close()
	defer two.close()
	ctx := context.Background()
	id, err := one.meta.CreateSession(ctx, Session{})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			meta := one.meta
			if index%2 == 0 {
				meta = two.meta
			}
			if err := meta.CommitRoundUsage(ctx, id, "same", Usage{2, 3, 5}, 2); err != nil {
				t.Error(err)
			}
		}(i)
	}
	workers.Wait()
	got, err := two.meta.GetSession(ctx, id)
	if err != nil || got.TotalTokens != 5 || got.MessageCount != 2 {
		t.Fatalf("shared SQLite ledger: %#v, %v", got, err)
	}
	if err := two.meta.SetLike(ctx, id+".m_reply", -1); err != nil {
		t.Fatal(err)
	}
	var like int
	if err := one.meta.(*SQLiteMetaStore).db.QueryRowContext(ctx, `SELECT like_status FROM msg_index WHERE msg_id=?`, id+".m_reply").Scan(&like); err != nil || like != -1 {
		t.Fatalf("SQL message index: %d %v", like, err)
	}
}

func TestRealMongoUniqueAtomicRounds(t *testing.T) {
	open := realContractFactory(t)
	one, two := open(), open()
	defer one.close()
	defer two.close()
	ctx := context.Background()
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := one.transcript.Append(ctx, "s_atomic", "append", []Message{{"role": "user", "content": "one"}, {"role": "assistant", "content": "two"}}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	collection := one.transcript.(*MongoTranscriptStore).messages
	count, err := collection.CountDocuments(ctx, bson.M{"sessionId": "s_atomic", "roundId": "append"})
	if err != nil || count != 1 {
		t.Fatalf("unique append envelope: %d %v", count, err)
	}
	messages, err := two.transcript.Latest(ctx, "s_atomic", 10)
	if err != nil || len(messages) != 2 {
		t.Fatal("unique index discarded part of a multi-message batch")
	}
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			store := one.transcript
			if index%2 == 0 {
				store = two.transcript
			}
			round, err := store.ReserveRound(ctx, "s_atomic_round", "round", strings.Repeat("c", 64), []Message{{"role": "user", "content": "ask"}}, 0)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- round.ReplyID
		}(i)
	}
	workers.Wait()
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("concurrent reservations created %d identities", len(unique))
	}
	count, err = collection.CountDocuments(ctx, bson.M{"sessionId": "s_atomic_round", "roundId": "round"})
	if err != nil || count != 1 {
		t.Fatal("reservation was not one document")
	}
	var committed, conflicts atomic.Int32
	for _, store := range []TranscriptStore{one.transcript, two.transcript} {
		workers.Add(1)
		go func(store TranscriptStore) {
			defer workers.Done()
			err := store.CommitRound(ctx, "s_atomic_round", "round", Message{"role": "assistant", "content": "answer"}, Usage{1, 1, 2}, []Event{{Type: EvText, Delta: "answer"}, {Type: EvDone, FinishReason: FinishStop}})
			if err == nil {
				committed.Add(1)
			} else if err == ErrStoreConflict {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}(store)
	}
	workers.Wait()
	if committed.Load() != 1 || conflicts.Load() != 1 {
		t.Fatal("pending-to-final CAS did not select a single winner")
	}
	if err := one.transcript.SaveSummary(ctx, "s_atomic", Summary{OriginStartID: "first", OriginEndID: "last", Content: "summary"}); err != nil {
		t.Fatal(err)
	}
	summaryCount, err := one.transcript.(*MongoTranscriptStore).summaries.CountDocuments(ctx, bson.M{"sessionId": "s_atomic"})
	if err != nil || summaryCount != 1 {
		t.Fatal("summary collection was not written")
	}
}

func TestRealRedisFlagsAndInbox(t *testing.T) {
	open := realContractFactory(t)
	one, two := open(), open()
	defer one.close()
	defer two.close()
	ctx := context.Background()
	producer, consumer := one.coord.(*RedisCoordinator), two.coord.(*RedisCoordinator)
	if err := producer.client.Set(ctx, producer.prefix+"feature_toggle:flag:compress_enabled", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if value, ok := consumer.Flag(ctx, "flag:compress_enabled"); !ok || value != "1" {
		t.Fatal("Redis feature flag did not cross clients")
	}
	hub := newEmitterHub()
	received := make(chan Event, 2)
	remove := hub.register("target", func(event Event) { received <- event })
	stop, err := consumer.SubscribeInbox(ctx, hub.dispatch)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := producer.Publish(ctx, "target", []byte(`{"type":"text","delta":"remote"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-received:
		if event.Delta != "remote" {
			t.Fatal("wrong chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbox did not reach emitter")
	}
	remove()
	if err := producer.Publish(ctx, "target", []byte(`{"type":"text","delta":"after-close"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
		t.Fatal("closed emitter received a chunk")
	case <-time.After(30 * time.Millisecond):
	}
	if err := producer.Publish(ctx, "target", []byte("invalid")); err != ErrStoreInvalid {
		t.Fatal("invalid event was published")
	}
}

func TestRealSSEInboxDelivery(t *testing.T) {
	stores := realContractFactory(t)()
	defer stores.close()
	previous := activeSessions
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	defer func() { activeSessions = previous }()
	coord := stores.coord.(*RedisCoordinator)
	stop, err := coord.SubscribeInbox(context.Background(), activeEmitters.dispatch)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_API_KEY", "fake-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"waiting"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	t.Setenv("LLM_BASE_URL", upstream.URL)
	backend := httptest.NewServer(http.HandlerFunc(handleChatStream))
	defer backend.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", backend.URL, strings.NewReader(`{"roundId":"inbox-sse","query":"wait","workspace":"/tmp"}`))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	emitterID := response.Header.Get("X-Emitter-Id")
	if emitterID == "" {
		t.Fatal("HTTP emitter not registered")
	}
	external, err := NewRedisCoordinator(ctx, os.Getenv("MINICOCO_TEST_REDIS_URL"), coord.prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	if err := external.Publish(ctx, emitterID, []byte(`{"type":"text","delta":"cross-node-sse"}`)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(response.Body)
	found := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "cross-node-sse") {
			found = true
			break
		}
	}
	cancel()
	response.Body.Close()
	if !found {
		t.Fatal("Redis inbox did not reach actual HTTP SSE writer")
	}
}

func TestRealRedisChild(t *testing.T) {
	mode := os.Getenv("MINICOCO_REALDB_CHILD_MODE")
	if mode == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	coord, err := NewRedisCoordinator(ctx, os.Getenv("MINICOCO_TEST_REDIS_URL"), os.Getenv("MINICOCO_REALDB_CHILD_PREFIX"))
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Close()
	if mode == "holder" {
		hub := newEmitterHub()
		received := make(chan Event, 1)
		remove := hub.register("cross-process", func(event Event) { received <- event })
		defer remove()
		stop, err := coord.SubscribeInbox(ctx, hub.dispatch)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		unlock, ok, err := coord.Lock(ctx, "lock:summary:shared", 5*time.Second)
		if err != nil || !ok {
			t.Fatal("holder could not lock")
		}
		defer unlock()
		fmt.Println("READY")
		select {
		case event := <-received:
			if event.Delta != "child-message" {
				t.Fatal("wrong cross-process message")
			}
			fmt.Println("DELIVERED")
		case <-ctx.Done():
			t.Fatal("child inbox timeout")
		}
	} else {
		unlock, ok, err := coord.Lock(ctx, "lock:summary:shared", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			unlock()
			t.Fatal("second process acquired held lock")
		}
		fmt.Println("BUSY")
		if err := coord.Publish(ctx, "cross-process", []byte(`{"type":"text","delta":"child-message"}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealRedisCrossProcess(t *testing.T) {
	stores := realContractFactory(t)()
	defer stores.close()
	coord := stores.coord.(*RedisCoordinator)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(mode string) *exec.Cmd {
		command := exec.Command(executable, "-test.run=^TestRealRedisChild$")
		command.Env = append(os.Environ(), "MINICOCO_REALDB_CHILD_MODE="+mode, "MINICOCO_REALDB_CHILD_PREFIX="+coord.prefix)
		return command
	}
	holder := child("holder")
	pipe, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- holder.Wait() }()
	t.Cleanup(func() {
		holder.Process.Kill() // Safe after Wait too; do not race on ProcessState.
	})
	ready := make(chan bool, 1)
	delivered := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		isReady, isDelivered := false, false
		for scanner.Scan() {
			if scanner.Text() == "READY" {
				isReady = true
				ready <- true
			}
			if scanner.Text() == "DELIVERED" {
				isDelivered = true
				delivered <- true
			}
		}
		if !isReady {
			ready <- false
		}
		if !isDelivered {
			delivered <- false
		}
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("holder failed before subscribing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder startup timeout")
	}
	output, err := child("publisher").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "BUSY") {
		t.Fatalf("publisher failed: %s %v", output, err)
	}
	select {
	case ok := <-delivered:
		if !ok {
			t.Fatal("message not delivered")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cross-process delivery timeout")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not exit")
	}
	unlock, ok, err := coord.Lock(context.Background(), "lock:summary:shared", time.Second)
	if err != nil || !ok {
		t.Fatal("holder did not release lock")
	}
	unlock()
}

func TestRealWiringAndCLI(t *testing.T) {
	t.Setenv("MINICOCO_MODEL_IS_FUNCTION_CALL", "")
	t.Setenv("MINICOCO_DEBUG_PROMPT", "0")
	stores := realContractFactory(t)()
	defer stores.close()
	transcript := stores.transcript.(*MongoTranscriptStore)
	coord := stores.coord.(*RedisCoordinator)
	t.Setenv("STORE_BACKEND", "real")
	t.Setenv("MONGO_URI", os.Getenv("MINICOCO_TEST_MONGO_URI"))
	t.Setenv("MONGO_DATABASE", transcript.database.Name())
	t.Setenv("REDIS_URL", os.Getenv("MINICOCO_TEST_REDIS_URL"))
	t.Setenv("REDIS_PREFIX", coord.prefix)
	t.Setenv("LLM_PROVIDER", "mock")
	preserveMockBudget(t)
	dir := t.TempDir()
	service, closeStores, err := wireStores(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStores()
	previous := activeSessions
	activeSessions = service
	defer func() { activeSessions = previous }()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/stream", handleChatStream)
	mux.HandleFunc("/sessions", handleSessions)
	mux.HandleFunc("/sessions/", handleSessions)
	backend := httptest.NewServer(mux)
	defer backend.Close()
	workspace := t.TempDir()
	run := func(args ...string) []byte {
		command := exec.Command("node", append([]string{"../client/src/cli.ts", "--server", backend.URL}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("realdb CLI failed: %s %v", output, err)
		}
		return output
	}
	run("--workspace", workspace, "inspect")
	sessions, err := service.meta.ListSessions(context.Background(), "")
	if err != nil || len(sessions) != 1 || sessions[0].MessageCount != 4 || sessions[0].TotalTokens != 4 {
		t.Fatalf("realdb loop: %#v %v", sessions, err)
	}
	sid := sessions[0].ID
	if output := run("--list"); !strings.Contains(string(output), sid) {
		t.Fatal("realdb CLI did not list session")
	}
	run("--resume", sid, "inspect again")
	session, err := service.meta.GetSession(context.Background(), sid)
	if err != nil || session.MessageCount != 8 || session.TotalTokens != 8 {
		t.Fatal("realdb resume did not read persisted history")
	}
	// Verify JSON replay goes through the same service and does not add usage.
	rounds, err := service.transcript.CommittedRounds(context.Background(), sid)
	if err != nil || len(rounds) != 4 {
		t.Fatal("missing durable realdb rounds")
	}
	encoded, err := json.Marshal(rounds[0].Events)
	if err != nil || len(encoded) == 0 {
		t.Fatal("realdb replay events missing")
	}
	// Reopen the assembled stores before replay, simulating a backend restart.
	closeStores()
	reopened, closeReopened, err := wireStores(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeReopened()
	activeSessions = reopened
	query, streaming := "inspect", false
	body, _ := json.Marshal(ChatRequest{SessionID: sid, RoundID: rounds[0].ID, Query: &query, Workspace: session.Workspace, Stream: &streaming})
	response, err := http.Post(backend.URL+"/chat/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var replay struct {
		Events []Event `json:"events"`
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
	if err != nil || response.StatusCode != 200 || json.Unmarshal(responseBody, &replay) != nil {
		t.Fatalf("realdb JSON replay after reopening stores: status=%d body=%s err=%v", response.StatusCode, responseBody, err)
	}
	replayed, _ := json.Marshal(replay.Events)
	if !bytes.Equal(encoded, replayed) {
		t.Fatal("realdb replay changed successful events")
	}
	after, err := reopened.meta.GetSession(context.Background(), sid)
	if err != nil || after.TotalTokens != session.TotalTokens || after.MessageCount != session.MessageCount {
		t.Fatal("realdb replay duplicated usage or messages")
	}
}

func TestRealXMLToolResultPersistence(t *testing.T) {
	open := realContractFactory(t)
	stores := open()
	defer func() { stores.close() }()
	previous := activeSessions
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	defer func() { activeSessions = previous }()
	t.Setenv("LLM_MODEL", "mock-xml")
	t.Setenv("MINICOCO_DEBUG_PROMPT", "0")
	t.Setenv("LLM_PROVIDER", "mock")
	preserveMockBudget(t)
	response, events := deltaResponse(t, map[string]any{"roundId": "real-xml-first", "query": "inspect", "workspace": "/tmp", "stream": false})
	requireRoundFinish(t, response, events, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	request := map[string]any{"sessionId": sid, "roundId": "real-xml-results", "workspace": "/tmp", "stream": false,
		"toolUseResults": []ToolUseResult{{ID: "call_mock_1", Name: "list_dir", Output: "missing <directory>", IsError: true}}}
	response, original := deltaResponse(t, request)
	requireRoundFinish(t, response, original, FinishStop)
	stores.close()
	stores = open()
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	history, err := stores.transcript.Latest(context.Background(), sid, 10)
	if err != nil || len(history) != 4 || history[2]["role"] != "user" || !strings.Contains(history[2]["content"].(string), "<status>failed</status>") {
		t.Fatalf("Mongo XML input did not survive reopen: %+v %v", history, err)
	}
	_, replay := deltaResponse(t, request)
	before, _ := json.Marshal(original)
	after, _ := json.Marshal(replay)
	if string(before) != string(after) {
		t.Fatal("real XML round replay changed events")
	}
	session, err := stores.meta.GetSession(context.Background(), sid)
	if err != nil || session.TotalTokens != 4 || session.MessageCount != 4 {
		t.Fatal("real XML replay duplicated usage/messages")
	}
}

func TestRealLoopDispatchAndCommittedReplay(t *testing.T) {
	open := realContractFactory(t)
	stores := open()
	defer func() { stores.close() }()
	previous := activeSessions
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	defer func() { activeSessions = previous }()
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LOOP_IMPL", "v1")
	preserveMockBudget(t)
	response, events := deltaResponse(t, map[string]any{"roundId": "real-loop-first", "query": "inspect", "workspace": "/tmp", "stream": true})
	requireRoundFinish(t, response, events, FinishToolCalls)
	sid := response.Header().Get("X-Session-Id")
	var calls atomic.Int32
	registerTestLoop(t, "test-v2", func(ctx context.Context, req ChatRequest, emit func(Event)) error {
		calls.Add(1)
		return processRequestV1(ctx, req, emit)
	})
	t.Setenv("LOOP_IMPL", "test-v2")
	request := map[string]any{"sessionId": sid, "roundId": "real-loop-result", "workspace": "/tmp", "stream": false, "toolUseResults": []ToolUseResult{{ID: "call_mock_1", Name: "list_dir", Output: "listing"}}}
	response, original := deltaResponse(t, request)
	requireRoundFinish(t, response, original, FinishStop)
	stores.close()
	stores = open()
	activeSessions = &sessionService{meta: stores.meta, transcript: stores.transcript, coord: stores.coord}
	t.Setenv("LLM_PROVIDER", "unknown") // Successful rounds must not call a model.
	response, replay := deltaResponse(t, request)
	requireRoundFinish(t, response, replay, FinishStop)
	before, _ := json.Marshal(original)
	after, _ := json.Marshal(replay)
	if calls.Load() != 2 || !bytes.Equal(before, after) {
		t.Fatal("real stores lost dispatch or durable events")
	}
	history, err := stores.transcript.Latest(context.Background(), sid, 10)
	if err != nil || len(history) != 4 {
		t.Fatal("implementation switch/reopen duplicated transcript", err)
	}
	session, err := stores.meta.GetSession(context.Background(), sid)
	if err != nil || session.TotalTokens != 4 || session.MessageCount != 4 {
		t.Fatal("implementation switch/replay duplicated usage", err)
	}
}

func TestRealCompressionPersistence(t *testing.T) { runCompressionPersistence(t, realContractFactory) }

func TestRealSubagentPersistence(t *testing.T) { runSubagentPersistence(t, realContractFactory) }
