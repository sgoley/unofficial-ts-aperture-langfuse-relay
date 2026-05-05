package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

type config struct {
	TSNetEnabled          bool
	TSNetTLSEnabled       bool
	TSNetHostname         string
	TSNetStateDir         string
	TSAuthKey             string
	ListenAddr            string
	HealthListenAddr      string
	WebhookPath           string
	ApertureAPIKey        string
	LangfuseBaseURL       string
	LangfusePublicKey     string
	LangfuseSecretKey     string
	LangfuseEnv           string
	RequestTimeout        time.Duration
	ShutdownTimeout       time.Duration
	MaxRequestBodySize    int64
	MaxLangfuseBatchBytes int64
	QueueSize             int
	WorkerCount           int
	RetryMaxAttempts      int
	RetryBaseDelay        time.Duration
	RetryMaxDelay         time.Duration
}

type aperturePayload struct {
	Metadata      apertureMetadata `json:"metadata"`
	RequestBody   json.RawMessage  `json:"request_body"`
	UserMessage   any              `json:"user_message"`
	ResponseBody  json.RawMessage  `json:"response_body"`
	RawResponses  json.RawMessage  `json:"raw_responses"`
	ToolCalls     json.RawMessage  `json:"tool_calls"`
	EstimatedCost *estimatedCost   `json:"estimated_cost"`
}

type apertureMetadata struct {
	LoginName      string         `json:"login_name"`
	UserAgent      string         `json:"user_agent"`
	URL            string         `json:"url"`
	Model          string         `json:"model"`
	Provider       string         `json:"provider"`
	TailnetName    string         `json:"tailnet_name"`
	StableNodeID   string         `json:"stable_node_id"`
	RequestID      string         `json:"request_id"`
	SessionID      string         `json:"session_id"`
	EstimatedCost  *estimatedCost `json:"estimated_cost"`
	RequestHeaders any            `json:"request_headers"`
}

type estimatedCost struct {
	Dollars   float64 `json:"dollars"`
	CostBasis string  `json:"cost_basis"`
	Usage     struct {
		InputTokens     int `json:"input_tokens"`
		OutputTokens    int `json:"output_tokens"`
		CachedTokens    int `json:"cached_tokens"`
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"usage"`
}

type langfuseEnvelope struct {
	Batch []any `json:"batch"`
}

type langfuseEvent struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Body      any    `json:"body"`
}

type ingestionResponse struct {
	Successes []struct {
		ID     string `json:"id"`
		Status int    `json:"status"`
	} `json:"successes"`
	Errors []struct {
		ID      string `json:"id"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	} `json:"errors"`
}

type server struct {
	cfg    config
	client *http.Client
	queue  chan langfuseJob
	ready  func(context.Context) bool
	wg     sync.WaitGroup
}

type langfuseJob struct {
	RequestID string
	Batch     []any
}

type retryableError struct {
	err error
}

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	s := newServer(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc(cfg.WebhookPath, s.handleApertureWebhook)

	httpServer := &http.Server{Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := startLocalHealthServer(ctx, cfg, s); err != nil {
		log.Fatal(err)
	}

	// Start workers once
	s.startWorkers()
	shutdownDone := make(chan struct{})

	// Shutdown handler
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		s.shutdownQueue(shutdownCtx)
		close(shutdownDone)
	}()

	if cfg.TSNetEnabled {
		// Start TSNet in a goroutine so we can also listen on Docker network
		go func() {
			ts := &tsnet.Server{
				Hostname: cfg.TSNetHostname,
				Dir:      cfg.TSNetStateDir,
				AuthKey:  cfg.TSAuthKey,
			}
			defer ts.Close()

			if err := ensureWritableDir(cfg.TSNetStateDir); err != nil {
				log.Printf("tsnet init error: %v", err)
				return
			}

			listen := ts.Listen
			scheme := "http"
			if cfg.TSNetTLSEnabled {
				listen = ts.ListenTLS
				scheme = "https"
			}

			ln, err := listen("tcp", cfg.ListenAddr)
			if err != nil {
				log.Printf("tsnet listen error: %v", err)
				return
			}
			s.ready = func(ctx context.Context) bool {
				lc, err := ts.LocalClient()
				if err != nil {
					return false
				}
				status, err := lc.Status(ctx)
				return err == nil && status != nil && status.BackendState == "Running"
			}
			log.Printf("relay listening on tailnet at %s://%s%s%s", scheme, cfg.TSNetHostname, cfg.ListenAddr, cfg.WebhookPath)

			if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("tsnet serve error: %v", err)
			}
		}()
	}

	// Always listen on regular network (Docker or host)
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("relay listening on %s%s", cfg.ListenAddr, cfg.WebhookPath)

	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
}

func newServer(cfg config) *server {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 100
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 2
	}
	return &server{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		queue: make(chan langfuseJob, cfg.QueueSize),
		ready: func(context.Context) bool { return true },
	}
}

func startLocalHealthServer(ctx context.Context, cfg config, app *server) error {
	if strings.TrimSpace(cfg.HealthListenAddr) == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.handleHealth)
	mux.HandleFunc("/readyz", app.handleReady)

	ln, err := net.Listen("tcp", cfg.HealthListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start local health listener on %s: %w", cfg.HealthListenAddr, err)
	}

	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("local health listener stopped: %v", err)
		}
	}()

	log.Printf("relay local health listening on http://%s", cfg.HealthListenAddr)
	return nil
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *server) handleReady(w http.ResponseWriter, r *http.Request) {
	if !s.ready(r.Context()) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

func (s *server) handleApertureWebhook(w http.ResponseWriter, r *http.Request) {
	log.Printf("webhook received: method=%s auth_header=%v", r.Method, r.Header.Get("Authorization") != "")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		log.Printf("webhook unauthorized")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var payload aperturePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	batch := buildLangfuseEvents(payload, s.cfg)
	if err := s.checkBatchSize(batch); err != nil {
		log.Printf("langfuse batch rejected request_id=%s err=%v", payload.Metadata.RequestID, err)
		http.Error(w, "payload too large for upstream ingestion", http.StatusRequestEntityTooLarge)
		return
	}
	if !s.enqueue(langfuseJob{RequestID: payload.Metadata.RequestID, Batch: batch}) {
		http.Error(w, "relay queue full", http.StatusServiceUnavailable)
		return
	}

	log.Printf("webhook accepted and enqueued: request_id=%s", payload.Metadata.RequestID)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) enqueue(job langfuseJob) bool {
	select {
	case s.queue <- job:
		return true
	default:
		return false
	}
}

func (s *server) startWorkers() {
	for i := 0; i < s.cfg.WorkerCount; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for job := range s.queue {
				if err := s.forwardWithRetry(job); err != nil {
					log.Printf("langfuse ingestion dropped request_id=%s err=%v", job.RequestID, err)
				}
			}
		}()
	}
}

func (s *server) shutdownQueue(ctx context.Context) {
	close(s.queue)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("relay shutdown timed out with queued work remaining")
	}
}

func (s *server) forwardWithRetry(job langfuseJob) error {
	log.Printf("forwarding to langfuse: request_id=%s batch_size=%d", job.RequestID, len(job.Batch))
	attempts := s.cfg.RetryMaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	delay := s.cfg.RetryBaseDelay
	if delay <= 0 {
		delay = 250 * time.Millisecond
	}
	maxDelay := s.cfg.RetryMaxDelay
	if maxDelay <= 0 {
		maxDelay = 5 * time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
		err := s.sendToLangfuse(ctx, job.Batch)
		cancel()
		if err == nil {
			log.Printf("langfuse ingestion succeeded: request_id=%s", job.RequestID)
			return nil
		}
		lastErr = err
		if !isRetryable(err) || attempt == attempts {
			break
		}
		log.Printf("langfuse ingestion failed (attempt %d/%d): request_id=%s err=%v", attempt, attempts, job.RequestID, err)
		time.Sleep(withJitter(delay))
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
	return lastErr
}

func isRetryable(err error) bool {
	var retryable retryableError
	return errors.As(err, &retryable)
}

func withJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	return delay + time.Duration(mrand.N(int64(delay/2)+1))
}

func (s *server) checkBatchSize(batch []any) error {
	if s.cfg.MaxLangfuseBatchBytes <= 0 {
		return nil
	}
	data, err := json.Marshal(langfuseEnvelope{Batch: batch})
	if err != nil {
		return err
	}
	if int64(len(data)) > s.cfg.MaxLangfuseBatchBytes {
		return fmt.Errorf("batch size %d exceeds max %d", len(data), s.cfg.MaxLangfuseBatchBytes)
	}
	return nil
}

func (s *server) sendToLangfuse(ctx context.Context, batch []any) error {
	data, err := json.Marshal(langfuseEnvelope{Batch: batch})
	if err != nil {
		return err
	}

	url := strings.TrimRight(s.cfg.LangfuseBaseURL, "/") + "/api/public/ingestion"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.cfg.LangfusePublicKey+":"+s.cfg.LangfuseSecretKey)))

	resp, err := s.client.Do(req)
	if err != nil {
		return retryableError{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		err := fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(msg))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			return retryableError{err: err}
		}
		return err
	}

	var parsed ingestionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if len(parsed.Errors) > 0 {
		return fmt.Errorf("ingestion errors: %s", formatIngestionErrors(parsed.Errors, batch))
	}

	return nil
}

func buildLangfuseEvents(p aperturePayload, cfg config) []any {
	traceID, generationID := makeLangfuseIDs(p.Metadata)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	est := p.EstimatedCost
	if est == nil {
		est = p.Metadata.EstimatedCost
	}

	requestBody := rawToAny(p.RequestBody)
	responseBody := rawToAny(p.ResponseBody)
	rawResponses := rawToAny(p.RawResponses)
	toolCalls := rawToAny(p.ToolCalls)

	traceMeta := map[string]any{
		"provider":        p.Metadata.Provider,
		"url":             p.Metadata.URL,
		"tailnet_name":    p.Metadata.TailnetName,
		"stable_node_id":  p.Metadata.StableNodeID,
		"user_agent":      p.Metadata.UserAgent,
		"request_headers": p.Metadata.RequestHeaders,
		"user_message":    p.UserMessage,
		"raw_responses":   rawResponses,
		"tool_calls":      toolCalls,
	}
	if est != nil {
		traceMeta["estimated_cost"] = est
	}

	traceBody := map[string]any{
		"id":          traceID,
		"timestamp":   now,
		"name":        pickNonEmpty("aperture/"+p.Metadata.Provider+"/"+p.Metadata.Model, "aperture/request"),
		"userId":      p.Metadata.LoginName,
		"sessionId":   p.Metadata.SessionID,
		"input":       primaryInput(requestBody, p.UserMessage),
		"output":      primaryOutput(responseBody, rawResponses),
		"metadata":    traceMeta,
		"environment": cfg.LangfuseEnv,
	}

	genBody := map[string]any{
		"id":          generationID,
		"traceId":     traceID,
		"name":        "aperture-generation",
		"startTime":   now,
		"endTime":     now,
		"model":       p.Metadata.Model,
		"input":       primaryInput(requestBody, p.UserMessage),
		"output":      primaryOutput(responseBody, rawResponses),
		"metadata":    traceMeta,
		"environment": cfg.LangfuseEnv,
	}

	if est != nil {
		genBody["costDetails"] = map[string]float64{"usd": est.Dollars}
		genBody["usage"] = map[string]int{
			"promptTokens":     est.Usage.InputTokens,
			"completionTokens": est.Usage.OutputTokens,
			"totalTokens":      est.Usage.InputTokens + est.Usage.OutputTokens,
		}
	}

	return []any{
		langfuseEvent{ID: newID(), Timestamp: now, Type: "trace-create", Body: traceBody},
		langfuseEvent{ID: newID(), Timestamp: now, Type: "generation-create", Body: genBody},
	}
}

func makeLangfuseIDs(metadata apertureMetadata) (string, string) {
	requestID := strings.TrimSpace(metadata.RequestID)
	if requestID == "" {
		traceID := newID()
		return traceID, newID()
	}

	return deterministicID(requestID, 16), deterministicID(requestID+":generation", 16)
}

func deterministicID(seed string, size int) string {
	if size <= 0 {
		size = 16
	}
	sum := sha256.Sum256([]byte(seed))
	if size > len(sum) {
		size = len(sum)
	}
	return hex.EncodeToString(sum[:size])
}

func primaryInput(requestBody any, userMessage any) any {
	return pickAny(requestBody, userMessage)
}

func primaryOutput(responseBody any, rawResponses any) any {
	return pickAny(responseBody, rawResponses)
}

func formatIngestionErrors(errors []struct {
	ID      string `json:"id"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}, batch []any) string {
	if len(errors) == 0 {
		return ""
	}

	batchIndex := make(map[string]langfuseEvent, len(batch))
	for _, item := range batch {
		event, ok := item.(langfuseEvent)
		if !ok {
			continue
		}
		batchIndex[event.ID] = event
	}

	details := make([]string, 0, len(errors))
	for _, item := range errors {
		detail := fmt.Sprintf("event_id=%s status=%d", item.ID, item.Status)
		if event, ok := batchIndex[item.ID]; ok {
			detail = fmt.Sprintf("%s type=%s body_id=%s", detail, event.Type, eventBodyID(event))
		}
		if item.Message != "" {
			detail = fmt.Sprintf("%s message=%s", detail, item.Message)
		}
		details = append(details, detail)
	}

	return strings.Join(details, "; ")
}

func eventBodyID(event langfuseEvent) string {
	body, ok := event.Body.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := body["id"].(string)
	return id
}

func (s *server) authorized(r *http.Request) bool {
	if s.cfg.ApertureAPIKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return constantTimeEqual(strings.TrimSpace(auth[7:]), s.cfg.ApertureAPIKey)
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	aHash := sha256.Sum256([]byte(a))
	bHash := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aHash[:], bHash[:]) == 1 && len(a) == len(b)
}

func ensureWritableDir(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	testFile, err := os.CreateTemp(path, ".write-test-*")
	if err != nil {
		return fmt.Errorf("tsnet state dir is not writable: %w", err)
	}
	name := testFile.Name()
	if err := testFile.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

func loadConfig() (config, error) {
	cfg := config{
		TSNetEnabled:          readBool("TSNET_ENABLED", true),
		TSNetTLSEnabled:       readBool("TSNET_TLS_ENABLED", false),
		TSNetHostname:         pick(os.Getenv("TSNET_HOSTNAME"), "aperture-langfuse-relay"),
		TSNetStateDir:         pick(os.Getenv("TSNET_STATE_DIR"), "./.tsnet"),
		TSAuthKey:             os.Getenv("TS_AUTHKEY"),
		ListenAddr:            pick(os.Getenv("LISTEN_ADDR"), ":8080"),
		HealthListenAddr:      pick(os.Getenv("HEALTH_LISTEN_ADDR"), ":8081"),
		WebhookPath:           pick(os.Getenv("WEBHOOK_PATH"), "/hooks/aperture"),
		ApertureAPIKey:        os.Getenv("APERTURE_API_KEY"),
		LangfuseBaseURL:       pick(os.Getenv("LANGFUSE_BASE_URL"), "https://cloud.langfuse.com"),
		LangfusePublicKey:     os.Getenv("LANGFUSE_PUBLIC_KEY"),
		LangfuseSecretKey:     os.Getenv("LANGFUSE_SECRET_KEY"),
		LangfuseEnv:           pick(os.Getenv("LANGFUSE_ENV"), "production"),
		RequestTimeout:        readDuration("REQUEST_TIMEOUT", 5*time.Second),
		ShutdownTimeout:       readDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
		MaxRequestBodySize:    int64(readInt("MAX_REQUEST_BODY_BYTES", 3*1024*1024)),
		MaxLangfuseBatchBytes: int64(readInt("MAX_LANGFUSE_BATCH_BYTES", 3*1024*1024)),
		QueueSize:             readInt("QUEUE_SIZE", 100),
		WorkerCount:           readInt("WORKER_COUNT", 2),
		RetryMaxAttempts:      readInt("RETRY_MAX_ATTEMPTS", 3),
		RetryBaseDelay:        readDuration("RETRY_BASE_DELAY", 250*time.Millisecond),
		RetryMaxDelay:         readDuration("RETRY_MAX_DELAY", 5*time.Second),
	}

	if cfg.LangfusePublicKey == "" || cfg.LangfuseSecretKey == "" {
		return config{}, errors.New("LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are required")
	}

	if !strings.HasPrefix(cfg.WebhookPath, "/") {
		cfg.WebhookPath = "/" + cfg.WebhookPath
	}

	return cfg, nil
}

func pick(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func pickNonEmpty(primary, fallback string) string {
	if strings.Contains(primary, "//") || strings.HasSuffix(primary, "/") {
		return fallback
	}
	return pick(primary, fallback)
}

func pickAny(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func rawToAny(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(v, &out); err != nil {
		return string(v)
	}
	return out
}

func newID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func readDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func readInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var out int
	if _, err := fmt.Sscanf(v, "%d", &out); err != nil || out <= 0 {
		return fallback
	}
	return out
}

func readBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes"
}
