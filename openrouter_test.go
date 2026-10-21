package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openRouterTestConfig(upstream string) config {
	return config{
		RelayMode: "openrouter", WebhookPath: "/hooks/aperture",
		OpenRouterPublicKey: "pk-relay", OpenRouterSecretKey: "sk-relay",
		LangfuseBaseURL: upstream, LangfusePublicKey: "pk-private", LangfuseSecretKey: "sk-private",
		MaxRequestBodySize: 1024, RequestTimeout: time.Second,
	}
}

func routerRequest(method, path string, body []byte, contentType string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.SetBasicAuth("pk-relay", "sk-relay")
	req.Header.Set("Content-Type", contentType)
	return req
}

func gzipBody(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOpenRouterForwardsPayloadAndReplacesCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, contentType, encoding string
		body                                      []byte
		status                                    int
	}{
		{"otel-json", "POST", otlpPath, "application/json", "", []byte(`{"resourceSpans":[]}`), 200},
		{"otel-protobuf", "POST", otlpPath, "application/x-protobuf", "", []byte{0x0a, 0x00}, 200},
		{"otel-gzip", "POST", otlpPath, "application/x-protobuf", "gzip", gzipBody(t, []byte{0x0a, 0x00}), 200},
		{"legacy", "POST", ingestionPath, "application/json; charset=utf-8", "", []byte(`{"batch":[]}`), 207},
		{"connection-check", "GET", projectsPath, "", "", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				u, p, ok := r.BasicAuth()
				if !ok || u != "pk-private" || p != "sk-private" {
					t.Error("upstream did not receive private project credentials")
				}
				if r.URL.Path != tc.path || r.Method != tc.method || r.URL.RawQuery != "" {
					t.Errorf("unexpected upstream route: %s %s", r.Method, r.URL)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, tc.body) {
					t.Error("upstream body was changed")
				}
				for _, name := range []string{"Cookie", "X-Forwarded-For", "X-Arbitrary-Secret"} {
					if r.Header.Get(name) != "" {
						t.Errorf("client header %s leaked upstream", name)
					}
				}
				if r.Header.Get("Content-Type") != tc.contentType || r.Header.Get("Content-Encoding") != tc.encoding || r.Header.Get("x-langfuse-ingestion-version") != "4" {
					t.Error("protocol headers were not preserved")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "private=session")
				w.Header().Set("Server", "private-origin")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":1}}`))
			}))
			defer upstream.Close()
			s := newServer(openRouterTestConfig(upstream.URL))
			req := routerRequest(tc.method, tc.path, tc.body, tc.contentType)
			req.Header.Set("Content-Encoding", tc.encoding)
			req.Header.Set("x-langfuse-ingestion-version", "4")
			req.Header.Set("Cookie", "client=secret")
			req.Header.Set("X-Forwarded-For", "spoofed")
			req.Header.Set("X-Arbitrary-Secret", "secret")
			res := httptest.NewRecorder()
			s.handler().ServeHTTP(res, req)
			if calls.Load() != 1 || res.Code != tc.status || res.Body.String() != `{"partialSuccess":{"rejectedSpans":1}}` {
				t.Fatalf("unexpected result: calls=%d status=%d body=%s", calls.Load(), res.Code, res.Body.String())
			}
			if res.Header().Get("Set-Cookie") != "" || res.Header().Get("Server") != "" {
				t.Fatal("private upstream headers leaked to sender")
			}
		})
	}
}

func TestOpenRouterRejectsRequestsBeforeContactingUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	s := newServer(openRouterTestConfig(upstream.URL))
	for _, tc := range []struct {
		name, method, path, contentType, auth, encoding string
		body                                            []byte
		status                                          int
	}{
		{"missing-auth", "POST", otlpPath, "application/json", "", "", nil, 401},
		{"wrong-secret", "POST", otlpPath, "application/json", "Basic cGstcmVsYXk6d3Jvbmc=", "", nil, 401},
		{"bearer-aperture", "POST", otlpPath, "application/json", "Bearer aperture-secret", "", nil, 401},
		{"ui", "GET", "/", "", "valid", "", nil, 404},
		{"signup", "POST", "/api/auth/signup", "application/json", "valid", "", nil, 404},
		{"read-traces", "GET", "/api/public/traces", "", "valid", "", nil, 404},
		{"project-write", "POST", projectsPath, "application/json", "valid", "", nil, 405},
		{"read-ingestion", "GET", ingestionPath, "", "valid", "", nil, 405},
		{"aperture-disabled", "POST", "/hooks/aperture", "application/json", "valid", "", nil, 404},
		{"query", "POST", otlpPath + "?url=http://other", "application/json", "valid", "", nil, 404},
		{"encoded-path", "POST", "/%61pi/public/otel/v1/traces", "application/json", "valid", "", nil, 404},
		{"traversal", "POST", "/api/public/otel/../projects", "application/json", "valid", "", nil, 404},
		{"trailing-slash", "POST", otlpPath + "/", "application/json", "valid", "", nil, 404},
		{"wrong-content-type", "POST", otlpPath, "text/plain", "valid", "", nil, 415},
		{"legacy-protobuf", "POST", ingestionPath, "application/x-protobuf", "valid", "", nil, 415},
		{"wrong-encoding", "POST", otlpPath, "application/json", "valid", "br", nil, 415},
		{"wire-limit", "POST", otlpPath, "application/json", "valid", "", bytes.Repeat([]byte("a"), 1025), 413},
		{"decoded-limit", "POST", otlpPath, "application/json", "valid", "gzip", gzipBody(t, bytes.Repeat([]byte("a"), 1025)), 413},
		{"invalid-gzip", "POST", otlpPath, "application/json", "valid", "gzip", []byte("invalid"), 400},
		{"project-body", "GET", projectsPath, "", "valid", "", []byte("unexpected"), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := routerRequest(tc.method, tc.path, tc.body, tc.contentType)
			if tc.auth != "valid" {
				req.Header.Set("Authorization", tc.auth)
			}
			req.Header.Set("Content-Encoding", tc.encoding)
			res := httptest.NewRecorder()
			s.handler().ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", res.Code, tc.status, res.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("blocked requests reached upstream %d times", calls.Load())
	}
}

func TestRelayModes(t *testing.T) {
	for _, mode := range []string{"", "aperture", "openrouter", "both"} {
		t.Run(mode, func(t *testing.T) {
			cfg := openRouterTestConfig("http://127.0.0.1:1")
			cfg.RelayMode, cfg.ApertureAPIKey = mode, "aperture-secret"
			cfg.MaxRequestBodySize = 4 * 1024 * 1024
			s := newServer(cfg)
			req := httptest.NewRequest("POST", "/hooks/aperture", bytes.NewReader(mustLoadFixturePayloadBytes(t)))
			req.Header.Set("Authorization", "Bearer aperture-secret")
			res := httptest.NewRecorder()
			s.handler().ServeHTTP(res, req)
			want := 202
			if mode == "openrouter" {
				want = 404
			}
			if res.Code != want {
				t.Fatalf("aperture status=%d want=%d", res.Code, want)
			}
			res = httptest.NewRecorder()
			// Missing credentials: enabled routes reject auth; disabled routes 404.
			s.handler().ServeHTTP(res, httptest.NewRequest("POST", otlpPath, nil))
			want = 404
			if mode == "both" || mode == "openrouter" {
				want = 401
			}
			if res.Code != want {
				t.Fatalf("openrouter status=%d want=%d", res.Code, want)
			}
		})
	}
}

func TestOpenRouterPreservesUpstreamFailures(t *testing.T) {
	for _, status := range []int{207, 400, 401, 429, 500, 503} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":["upstream-error"]}`))
		}))
		s := newServer(openRouterTestConfig(upstream.URL))
		res := httptest.NewRecorder()
		s.handler().ServeHTTP(res, routerRequest("POST", otlpPath, []byte("{}"), "application/json"))
		upstream.Close()
		if res.Code != status || res.Header().Get("Retry-After") != "7" || !strings.Contains(res.Body.String(), "upstream-error") {
			t.Fatalf("lost upstream status/body/retry-after for %d: %+v", status, res)
		}
	}
}

func TestOpenRouterRefusesRedirectsAndOversizedResponses(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, redirect := range []bool{true, false} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				http.Redirect(w, r, target.URL, 307)
				return
			}
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxResponseSize+1))
		}))
		s := newServer(openRouterTestConfig(upstream.URL))
		res := httptest.NewRecorder()
		s.handler().ServeHTTP(res, routerRequest("POST", otlpPath, []byte("{}"), "application/json"))
		upstream.Close()
		if res.Code != 502 || res.Header().Get("Location") != "" {
			t.Fatalf("expected bounded 502, got %d", res.Code)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("credentials/body were sent to redirect target")
	}
}

func TestOpenRouterConcurrencyLimitAndCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	cfg := openRouterTestConfig(upstream.URL)
	cfg.OpenRouterMaxInFlight = 1
	s := newServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handler().ServeHTTP(httptest.NewRecorder(), routerRequest("POST", otlpPath, []byte("{}"), "application/json").WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never started")
	}
	res := httptest.NewRecorder()
	s.handler().ServeHTTP(res, routerRequest("POST", otlpPath, []byte("{}"), "application/json"))
	if res.Code != 503 || res.Header().Get("Retry-After") != "1" {
		t.Fatalf("busy relay status=%d", res.Code)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not cancelled")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay request did not finish")
	}
}

func TestRelayConfigurationValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		valid bool
	}{
		{"legacy-default", nil, true},
		{"openrouter", map[string]string{"RELAY_MODE": "openrouter"}, true},
		{"tailnet-upstream", map[string]string{"LANGFUSE_USE_TSNET": "true", "TSNET_ENABLED": "true"}, true},
		{"tailnet-without-node", map[string]string{"LANGFUSE_USE_TSNET": "true", "TSNET_ENABLED": "false"}, false},
		{"both", map[string]string{"RELAY_MODE": "both"}, true},
		{"bad-mode", map[string]string{"RELAY_MODE": "invalid"}, false},
		{"missing-secret", map[string]string{"RELAY_MODE": "openrouter", "OPENROUTER_RELAY_SECRET_KEY": ""}, false},
		{"missing-public", map[string]string{"RELAY_MODE": "openrouter", "OPENROUTER_RELAY_PUBLIC_KEY": ""}, false},
		{"both-needs-aperture-auth", map[string]string{"RELAY_MODE": "both", "APERTURE_API_KEY": ""}, false},
		{"colon-public-key", map[string]string{"OPENROUTER_RELAY_PUBLIC_KEY": "bad:key"}, false},
		{"bad-upstream", map[string]string{"LANGFUSE_BASE_URL": "file:///tmp/data"}, false},
		{"query-upstream", map[string]string{"LANGFUSE_BASE_URL": "https://example.org?redirect=elsewhere"}, false},
		{"embedded-credentials", map[string]string{"LANGFUSE_BASE_URL": "https://user:secret@example.org"}, false},
		{"reserved-hook", map[string]string{"WEBHOOK_PATH": otlpPath}, false},
		{"zero-timeout", map[string]string{"REQUEST_TIMEOUT": "0s"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range map[string]string{
				"RELAY_MODE": "aperture", "OPENROUTER_RELAY_PUBLIC_KEY": "pk-relay", "OPENROUTER_RELAY_SECRET_KEY": "sk-relay",
				"APERTURE_API_KEY": "aperture-secret", "LANGFUSE_BASE_URL": "https://example.org", "LANGFUSE_PUBLIC_KEY": "pk-private", "LANGFUSE_SECRET_KEY": "sk-private",
				"WEBHOOK_PATH": "/hooks/aperture", "REQUEST_TIMEOUT": "5s", "SHUTDOWN_TIMEOUT": "10s",
				"LANGFUSE_USE_TSNET": "false", "TSNET_ENABLED": "false",
			} {
				t.Setenv(key, value)
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			_, err := loadConfig()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, err)
			}
		})
	}
}

func TestTailnetTransportUsesNodeDialerForBothSources(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		if r.URL.Path == ingestionPath {
			w.WriteHeader(207)
			_, _ = w.Write([]byte(`{"successes":[],"errors":[]}`))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()
	cfg := openRouterTestConfig("http://private-langfuse:3000")
	s := newServer(cfg)
	var dials atomic.Int32
	s.useTailnetDialer(func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if network != "tcp" || address != "private-langfuse:3000" {
			t.Errorf("wrong dial target: %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	})
	transport := s.client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil {
		t.Fatal("tailnet upstream must not use environment proxies")
	}
	res := httptest.NewRecorder()
	s.handler().ServeHTTP(res, routerRequest("POST", otlpPath, []byte("{}"), "application/json"))
	if res.Code != 200 {
		t.Fatalf("OTLP tailnet forwarding failed: %d", res.Code)
	}
	if err := s.sendToLangfuse(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || dials.Load() == 0 {
		t.Fatal("both source paths must use the configured tailnet client")
	}
}
