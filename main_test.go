package main

import (
	"bytes"
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

	genBody, ok := genEvent.Body.(map[string]any)
	if !ok {
		t.Fatalf("generation body should be object")
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

func TestHandleApertureWebhook_Smoke(t *testing.T) {
	payload := mustLoadFixturePayloadBytes(t)

	var gotEnvelope langfuseEnvelope
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
	}))
	defer langfuse.Close()

	s := &server{
		cfg: config{
			WebhookPath:        "/hooks/aperture",
			ApertureAPIKey:     "hook-secret",
			LangfuseBaseURL:    langfuse.URL,
			LangfusePublicKey:  "pk-test",
			LangfuseSecretKey:  "sk-test",
			LangfuseEnv:        "test",
			MaxRequestBodySize: 4 * 1024 * 1024,
		},
		client: &http.Client{Timeout: 5 * time.Second},
	}

	req := httptest.NewRequest(http.MethodPost, "/hooks/aperture", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer hook-secret")
	res := httptest.NewRecorder()

	s.handleApertureWebhook(res, req)

	if res.Code != http.StatusAccepted {
		t.Fatalf("expected 202 from webhook, got %d: %s", res.Code, res.Body.String())
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
	s := &server{
		cfg: config{
			ApertureAPIKey:     "expected-secret",
			MaxRequestBodySize: 1024,
		},
		client: &http.Client{Timeout: 2 * time.Second},
	}

	req := httptest.NewRequest(http.MethodPost, "/hooks/aperture", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	res := httptest.NewRecorder()

	s.handleApertureWebhook(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
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
