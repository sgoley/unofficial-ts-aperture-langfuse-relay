package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildLangfuseEvents_MapsFixture(t *testing.T) {
	payload := mustLoadFixturePayload(t)

	cfg := config{LangfuseEnv: "test"}
	events := buildLangfuseEvents(payload, cfg)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	traceEvent, ok := events[0].(langfuseEvent)
	if !ok {
		t.Fatalf("first event should be langfuseEvent")
	}
	if traceEvent.Type != "trace-create" {
		t.Fatalf("expected trace-create, got %s", traceEvent.Type)
	}

	genEvent, ok := events[1].(langfuseEvent)
	if !ok {
		t.Fatalf("second event should be langfuseEvent")
	}
	if genEvent.Type != "generation-create" {
		t.Fatalf("expected generation-create, got %s", genEvent.Type)
	}

	traceBody, ok := traceEvent.Body.(map[string]any)
	if !ok {
		t.Fatalf("trace body should be object")
	}
	if traceBody["userId"] != "alice@example.com" {
		t.Fatalf("expected userId to map from login_name")
	}
	if traceBody["sessionId"] != "session-xyz-999" {
		t.Fatalf("expected sessionId to map from session_id")
	}
	input, ok := traceBody["input"].(map[string]any)
	if !ok {
		t.Fatalf("expected trace input to preserve structured request_body")
	}
	if _, ok := input["messages"].([]any); !ok {
		t.Fatalf("expected request_body messages to be preserved in trace input")
	}

	genBody, ok := genEvent.Body.(map[string]any)
	if !ok {
		t.Fatalf("generation body should be object")
	}
	metadata, ok := genBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("generation metadata should be object")
	}
	if metadata["user_message"] != "Explain cache invalidation in one sentence" {
		t.Fatalf("expected user_message to be preserved in metadata")
	}
	usage, ok := genBody["usage"].(map[string]int)
	if !ok {
		t.Fatalf("usage should be map[string]int")
	}
	if usage["promptTokens"] != 120 || usage["completionTokens"] != 40 || usage["totalTokens"] != 160 {
		t.Fatalf("unexpected usage mapping: %+v", usage)
	}

	costDetails, ok := genBody["costDetails"].(map[string]float64)
	if !ok {
		t.Fatalf("costDetails should be map[string]float64")
	}
	if costDetails["usd"] != 0.0012 {
		t.Fatalf("expected usd cost to map from estimated_cost")
	}
}

func TestBuildLangfuseEvents_MissingRequestIDGeneratesUniqueIDs(t *testing.T) {
	payload := mustLoadFixturePayload(t)
	payload.Metadata.RequestID = ""

	eventsOne := buildLangfuseEvents(payload, config{LangfuseEnv: "test"})
	eventsTwo := buildLangfuseEvents(payload, config{LangfuseEnv: "test"})

	traceOne := mustEventBody(t, eventsOne[0])
	genOne := mustEventBody(t, eventsOne[1])
	traceTwo := mustEventBody(t, eventsTwo[0])
	genTwo := mustEventBody(t, eventsTwo[1])

	traceOneID := mustStringField(t, traceOne, "id")
	genOneID := mustStringField(t, genOne, "id")
	traceTwoID := mustStringField(t, traceTwo, "id")
	genTwoID := mustStringField(t, genTwo, "id")

	if traceOneID == "" || genOneID == "" {
		t.Fatalf("expected generated IDs when request_id is missing")
	}
	if genOneID == "-gen" {
		t.Fatalf("generation ID must not use broken literal fallback")
	}
	if traceOneID == genOneID {
		t.Fatalf("trace and generation IDs should be distinct")
	}
	if traceOneID == traceTwoID || genOneID == genTwoID {
		t.Fatalf("IDs should be unique across requests without request_id")
	}
}

func TestHandleApertureWebhook_Smoke(t *testing.T) {
	payload := mustLoadFixturePayloadBytes(t)

	var gotEnvelope langfuseEnvelope
	received := make(chan struct{})
	langfuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/ingestion" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-test:sk-test"))
		if auth != expectedAuth {
			t.Fatalf("unexpected auth header: %s", auth)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %s", ct)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotEnvelope); err != nil {
			t.Fatalf("invalid ingestion envelope: %v", err)
		}
		if len(gotEnvelope.Batch) != 2 {
			t.Fatalf("expected 2 batch events, got %d", len(gotEnvelope.Batch))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`{"successes":[{"id":"ok-1","status":201}],"errors":[]}`))
		close(received)
	}))
	defer langfuse.Close()

	s := newServer(config{
		WebhookPath:           "/hooks/aperture",
		ApertureAPIKey:        "hook-secret",
		LangfuseBaseURL:       langfuse.URL,
		LangfusePublicKey:     "pk-test",
		LangfuseSecretKey:     "sk-test",
		LangfuseEnv:           "test",
		MaxRequestBodySize:    4 * 1024 * 1024,
		MaxLangfuseBatchBytes: 3 * 1024 * 1024,
		RequestTimeout:        5 * time.Second,
		RetryMaxAttempts:      1,
	})
	s.startWorkers()
	defer s.shutdownQueue(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/hooks/aperture", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer hook-secret")
	res := httptest.NewRecorder()

	s.handleApertureWebhook(res, req)

	if res.Code != http.StatusAccepted {
		t.Fatalf("expected 202 from webhook, got %d: %s", res.Code, res.Body.String())
	}

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for async Langfuse ingestion")
	}

	batchBytes, err := json.Marshal(gotEnvelope.Batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if !bytes.Contains(batchBytes, []byte(`"trace-create"`)) {
		t.Fatalf("expected trace-create in batch payload")
	}
	if !bytes.Contains(batchBytes, []byte(`"generation-create"`)) {
		t.Fatalf("expected generation-create in batch payload")
	}
}

func TestHandleApertureWebhook_Unauthorized(t *testing.T) {
	s := newServer(config{
		ApertureAPIKey:     "expected-secret",
		MaxRequestBodySize: 1024,
	})

	req := httptest.NewRequest(http.MethodPost, "/hooks/aperture", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("x-api-key", "expected-secret")
	res := httptest.NewRecorder()

	s.handleApertureWebhook(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
}

func TestHandleApertureWebhook_QueueFull(t *testing.T) {
	s := newServer(config{
		ApertureAPIKey:        "hook-secret",
		LangfuseBaseURL:       "http://127.0.0.1:1",
		MaxRequestBodySize:    4 * 1024 * 1024,
		MaxLangfuseBatchBytes: 3 * 1024 * 1024,
		QueueSize:             1,
	})
	s.queue <- langfuseJob{RequestID: "already-full"}

	req := httptest.NewRequest(http.MethodPost, "/hooks/aperture", bytes.NewReader(mustLoadFixturePayloadBytes(t)))
	req.Header.Set("Authorization", "Bearer hook-secret")
	res := httptest.NewRecorder()

	s.handleApertureWebhook(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when queue is full, got %d", res.Code)
	}
}

func TestHandleApertureWebhook_BatchTooLarge(t *testing.T) {
	s := newServer(config{
		ApertureAPIKey:        "hook-secret",
		MaxRequestBodySize:    4 * 1024 * 1024,
		MaxLangfuseBatchBytes: 10,
	})

	req := httptest.NewRequest(http.MethodPost, "/hooks/aperture", bytes.NewReader(mustLoadFixturePayloadBytes(t)))
	req.Header.Set("Authorization", "Bearer hook-secret")
	res := httptest.NewRecorder()

	s.handleApertureWebhook(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 when batch is too large, got %d", res.Code)
	}
}

func TestForwardWithRetry_RetriesRetryableStatus(t *testing.T) {
	attempts := 0
	langfuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
	defer langfuse.Close()

	s := newServer(config{
		LangfuseBaseURL:   langfuse.URL,
		LangfusePublicKey: "pk-test",
		LangfuseSecretKey: "sk-test",
		RequestTimeout:    2 * time.Second,
		RetryMaxAttempts:  2,
		RetryBaseDelay:    time.Millisecond,
		RetryMaxDelay:     time.Millisecond,
	})

	if err := s.forwardWithRetry(langfuseJob{RequestID: "retry", Batch: buildLangfuseEvents(mustLoadFixturePayload(t), config{LangfuseEnv: "test"})}); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestHandleReady(t *testing.T) {
	s := newServer(config{})
	s.ready = func(context.Context) bool { return false }

	res := httptest.NewRecorder()
	s.handleReady(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when not ready, got %d", res.Code)
	}

	s.ready = func(context.Context) bool { return true }
	res = httptest.NewRecorder()
	s.handleReady(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 when ready, got %d", res.Code)
	}
}

func TestSendToLangfuse_PartialFailureIncludesEventDetails(t *testing.T) {
	payload := mustLoadFixturePayload(t)
	batch := buildLangfuseEvents(payload, config{LangfuseEnv: "test"})
	eventID := batch[1].(langfuseEvent).ID

	langfuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`{"successes":[],"errors":[{"id":"` + eventID + `","status":400,"message":"bad generation"}]}`))
	}))
	defer langfuse.Close()

	s := &server{
		cfg: config{
			LangfuseBaseURL:   langfuse.URL,
			LangfusePublicKey: "pk-test",
			LangfuseSecretKey: "sk-test",
		},
		client: &http.Client{Timeout: 5 * time.Second},
	}

	err := s.sendToLangfuse(t.Context(), batch)
	if err == nil {
		t.Fatalf("expected partial failure error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("generation-create")) {
		t.Fatalf("expected error details to include failed event type: %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("bad generation")) {
		t.Fatalf("expected error details to include upstream message: %v", err)
	}
}

func mustLoadFixturePayload(t *testing.T) aperturePayload {
	t.Helper()
	b := mustLoadFixturePayloadBytes(t)
	var payload aperturePayload
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("parse fixture payload: %v", err)
	}
	return payload
}

func mustLoadFixturePayloadBytes(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join("testdata", "aperture_payload.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", p, err)
	}
	return b
}

func mustEventBody(t *testing.T, event any) map[string]any {
	t.Helper()
	parsed, ok := event.(langfuseEvent)
	if !ok {
		t.Fatalf("event should be langfuseEvent")
	}
	body, ok := parsed.Body.(map[string]any)
	if !ok {
		t.Fatalf("event body should be map")
	}
	return body
}

func mustStringField(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	v, ok := body[key].(string)
	if !ok {
		t.Fatalf("expected %s to be string", key)
	}
	return v
}
