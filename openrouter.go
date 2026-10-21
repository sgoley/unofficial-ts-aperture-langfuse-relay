package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
)

const (
	otlpPath        = "/api/public/otel/v1/traces"
	ingestionPath   = "/api/public/ingestion"
	projectsPath    = "/api/public/projects"
	maxResponseSize = 1024 * 1024
)

// Use literal paths rather than a catch-all reverse proxy. No UI, project
// administration, trace reads, arbitrary URLs, or path normalization redirects.
func (s *server) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == "/healthz":
			s.handleHealth(w, r)
		case r.URL.Path == "/readyz":
			s.handleReady(w, r)
		case r.URL.Path == s.cfg.WebhookPath && s.cfg.RelayMode != "openrouter":
			s.handleApertureWebhook(w, r)
		case s.cfg.RelayMode == "openrouter" || s.cfg.RelayMode == "both":
			s.handleOpenRouter(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// OpenRouter's Langfuse destination already emits Langfuse/OTLP payloads.
// Forward them unchanged rather than applying the Aperture transformation.
func (s *server) handleOpenRouter(w http.ResponseWriter, r *http.Request) {
	expectedMethod := http.MethodPost
	switch r.URL.Path {
	case otlpPath, ingestionPath:
	case projectsPath:
		// A project-scoped credential check; never allow project writes or reads
		// of traces. Keep this for destination connection-test compatibility.
		expectedMethod = http.MethodGet
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != expectedMethod {
		w.Header().Set("Allow", expectedMethod)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	username, password, ok := r.BasicAuth()
	userMatches := constantTimeEqual(username, s.cfg.OpenRouterPublicKey)
	secretMatches := constantTimeEqual(password, s.cfg.OpenRouterSecretKey)
	if !ok || !userMatches || !secretMatches || s.cfg.OpenRouterPublicKey == "" || s.cfg.OpenRouterSecretKey == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="langfuse-relay"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case s.proxySlots <- struct{}{}:
		defer func() { <-s.proxySlots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "relay busy", http.StatusServiceUnavailable)
		return
	}

	if r.Method == http.MethodPost {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || (mediaType != "application/json" && (r.URL.Path != otlpPath || mediaType != "application/x-protobuf")) {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
	}
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return
	}
	// Bound both wire size and decompressed size while preserving the original
	// payload bytes for Langfuse.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid request body", http.StatusBadRequest)
		}
		return
	}
	if r.Method == http.MethodGet && len(body) != 0 {
		http.Error(w, "connection checks must not include a body", http.StatusBadRequest)
		return
	}
	if encoding == "gzip" {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return
		}
		size, readErr := io.Copy(io.Discard, io.LimitReader(reader, s.cfg.MaxRequestBodySize+1))
		_ = reader.Close()
		if size > s.cfg.MaxRequestBodySize {
			http.Error(w, "decoded payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		if readErr != nil {
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return
		}
	}

	upstreamURL := strings.TrimRight(s.cfg.LangfuseBaseURL, "/") + r.URL.Path
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	// Only forward protocol headers. Client credentials, cookies, forwarded
	// identity headers and arbitrary destination headers never reach Langfuse.
	for _, name := range []string{"Content-Type", "Content-Encoding", "Accept", "x-langfuse-ingestion-version"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	req.SetBasicAuth(s.cfg.LangfusePublicKey, s.cfg.LangfuseSecretKey)
	resp, err := s.client.Do(req)
	if err != nil {
		// Do not log a client body, credentials, or a private upstream address.
		log.Print("openrouter upstream request failed")
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		http.Error(w, "upstream redirect refused", http.StatusBadGateway)
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil || len(responseBody) > maxResponseSize {
		http.Error(w, "invalid upstream response", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Retry-After"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Preserve OTLP partial-success responses, legacy 207 errors, and upstream
	// 429/5xx statuses so the sender can make its own retry decisions. Do not
	// acknowledge until Langfuse responds, and do not retry writes here.
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	log.Printf("openrouter forwarded method=%s path=%s status=%d", r.Method, r.URL.Path, resp.StatusCode)
}
