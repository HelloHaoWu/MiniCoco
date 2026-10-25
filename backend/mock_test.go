package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func preserveMockBudget(t *testing.T) {
	t.Helper()
	previous := flakyRemaining.Load()
	flakyRemaining.Store(0)
	t.Cleanup(func() { flakyRemaining.Store(previous) })
}

func decodeEvents(t *testing.T, body io.Reader) []Event {
	t.Helper()
	var events []Event
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data: ") {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func assertMockRound(t *testing.T, events []Event, attempt int) {
	t.Helper()
	if attempt < 2 {
		if len(events) != 2 || events[0].Type != EvError || events[0].Code != codeUpstream5xx || events[0].Retryable == nil || !*events[0].Retryable || events[1].FinishReason != "error" {
			t.Fatalf("attempt %d was not a classified mock failure: %+v", attempt+1, events)
		}
		return
	}
	finish := FinishToolCalls
	if attempt == 3 {
		finish = FinishStop
	}
	if len(events) == 0 || events[len(events)-1].FinishReason != finish {
		t.Fatalf("unexpected successful mock round: %+v", events)
	}
	tools := 0
	for _, event := range events {
		if event.Type == EvError || event.Code != "" || event.Retryable != nil {
			t.Fatalf("successful event changed: %+v", event)
		}
		if event.Type == EvToolCall {
			tools++
		}
	}
	if (attempt == 2 && tools != 1) || (attempt == 3 && tools != 0) {
		t.Fatal("incorrect mock tool cycle")
	}
}

func TestFlakyMockHTTPRounds(t *testing.T) {
	sessionFixture(t)
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LLM_MOCK_MODE", "flaky")
	t.Setenv("LLM_MOCK_FLAKY_FAILS", "2")
	if err := configureMockFaults(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		health := httptest.NewRecorder()
		handleHealth(health, httptest.NewRequest("GET", "/health", nil))
		body := `{"roundId":"mock-first","query":"test"}`
		if attempt == 3 {
			body = fmt.Sprintf(`{"sessionId":%q,"roundId":"mock-tools","toolUseResults":[{"id":"call_mock_1","name":"list_dir","output":"listing","isError":false}]}`, initialSessionID("mock-first"))
		}
		response := httptest.NewRecorder()
		handleChatStream(response, httptest.NewRequest("POST", "/chat/stream", strings.NewReader(body)))
		assertMockRound(t, decodeEvents(t, response.Body), attempt)
	}
	if flakyRemaining.Load() != 0 {
		t.Fatal("failure budget did not drain to zero")
	}
}

func TestFlakyMockConcurrentBudget(t *testing.T) {
	preserveMockBudget(t)
	t.Setenv("LLM_PROVIDER", "mock")
	t.Setenv("LLM_MOCK_MODE", "flaky")
	t.Setenv("LLM_MOCK_FLAKY_FAILS", "7")
	if err := configureMockFaults(); err != nil {
		t.Fatal(err)
	}
	var failures, successes atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 64; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			processModelContext(context.Background(), nil, "", "", func(event Event) {
				if event.Type == EvError {
					failures.Add(1)
				}
				if event.Type == EvDone && event.FinishReason == FinishToolCalls {
					successes.Add(1)
				}
			})
		}()
	}
	group.Wait()
	if failures.Load() != 7 || successes.Load() != 57 || flakyRemaining.Load() != 0 {
		t.Fatalf("incorrect concurrent budget: failures=%d successes=%d remaining=%d", failures.Load(), successes.Load(), flakyRemaining.Load())
	}
}

func TestMockConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, provider, mode, count string
		want                        int64
		invalid                     bool
	}{
		{"normal", "mock", "", "2", 0, false},
		{"default count", "mock", "flaky", "", 2, false},
		{"zero failures", "mock", "flaky", "0", 0, false},
		{"configured count", "mock", "flaky", "5", 5, false},
		{"negative count", "mock", "flaky", "-1", 0, true},
		{"malformed count", "mock", "flaky", "bad", 0, true},
		{"overflow count", "mock", "flaky", "99999999999999999999", 0, true},
		{"unknown mode", "mock", "unknown", "2", 0, true},
		{"real provider ignores mock settings", "openai", "unknown", "bad", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveMockBudget(t)
			t.Setenv("LLM_PROVIDER", test.provider)
			t.Setenv("LLM_MOCK_MODE", test.mode)
			t.Setenv("LLM_MOCK_FLAKY_FAILS", test.count)
			err := configureMockFaults()
			if (err != nil) != test.invalid || flakyRemaining.Load() != test.want {
				t.Fatalf("invalid config outcome: error=%v remaining=%d", err, flakyRemaining.Load())
			}
			if err != nil {
				code, retryable := classifyCompletionError(err)
				if code != codeConfig || retryable {
					t.Fatal("mock configuration failure must be fatal")
				}
			}
		})
	}
}

// The child executes the production startup path, including .env loading and
// configureMockFaults, using the already-built test binary in a fresh process.
func TestFlakyBackendChild(t *testing.T) {
	if os.Getenv("MINICOCO_TEST_FLAKY_CHILD") != "1" {
		return
	}
	main()
}

func TestFlakyMockProcessIsolation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for instance := 0; instance < 2; instance++ {
		t.Run(fmt.Sprintf("process-%d", instance+1), func(t *testing.T) {
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := reservation.Addr().(*net.TCPAddr).Port
			reservation.Close()
			command := exec.Command(executable, "-test.run=^TestFlakyBackendChild$")
			command.Dir = t.TempDir()
			command.Env = append(os.Environ(), "STORE_BACKEND=file", "MINICOCO_DATA_DIR="+command.Dir+"/data", "MINICOCO_TEST_FLAKY_CHILD=1", "LLM_PROVIDER=mock", "LLM_API_KEY=", "LLM_MOCK_MODE=flaky", "LLM_MOCK_FLAKY_FAILS=2", "MINICOCO_HOST=127.0.0.1", fmt.Sprintf("MINICOCO_PORT=%d", port))
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { command.Process.Kill(); command.Wait() })
			base := fmt.Sprintf("http://127.0.0.1:%d", port)
			client := &http.Client{Timeout: 300 * time.Millisecond}
			ready := false
			for i := 0; i < 100; i++ {
				response, err := client.Get(base + "/health")
				if err == nil {
					response.Body.Close()
					if response.StatusCode == 200 {
						ready = true
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !ready {
				t.Fatal("isolated backend failed to start")
			}
			for attempt := 0; attempt < 4; attempt++ {
				body := `{"roundId":"mock-first","query":"test"}`
				if attempt == 3 {
					body = fmt.Sprintf(`{"sessionId":%q,"roundId":"mock-tools","toolUseResults":[{"id":"call_mock_1","name":"list_dir","output":"listing","isError":false}]}`, initialSessionID("mock-first"))
				}
				response, err := client.Post(base+"/chat/stream", "application/json", bytes.NewBufferString(body))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(io.LimitReader(response.Body, 32*1024))
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 {
					t.Fatalf("unexpected HTTP status %d", response.StatusCode)
				}
				assertMockRound(t, decodeEvents(t, bytes.NewReader(data)), attempt)
			}
		})
	}
}
