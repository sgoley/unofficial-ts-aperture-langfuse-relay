package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

type config struct {
	TSNetEnabled       bool
	TSNetHostname      string
	TSNetStateDir      string
	TSAuthKey          string
	ListenAddr         string
	WebhookPath        string
	ApertureAPIKey     string
	LangfuseBaseURL    string
	LangfusePublicKey  string
	LangfuseSecretKey  string
	LangfuseEnv        string
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
	MaxRequestBodySize int64
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
		ID string `json:"id"`
	} `json:"successes"`
	Errors []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"errors"`
}

type server struct {
	cfg    config
	client *http.Client
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc(cfg.WebhookPath, s.handleApertureWebhook)

	httpServer := &http.Server{Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.TSNetEnabled {
		if err := serveTSNet(ctx, cfg, httpServer); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("relay listening on %s%s", cfg.ListenAddr, cfg.WebhookPath)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func serveTSNet(ctx context.Context, cfg config, httpServer *http.Server) error {
	ts := &tsnet.Server{
		Hostname: cfg.TSNetHostname,
		Dir:      cfg.TSNetStateDir,
		AuthKey:  cfg.TSAuthKey,
	}
	defer ts.Close()

	ln, err := ts.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Printf("relay listening on tailnet at http://%s%s%s", cfg.TSNetHostname, cfg.ListenAddr, cfg.WebhookPath)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	return httpServer.Serve(ln)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *server) handleApertureWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
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
	if err := s.sendToLangfuse(r.Context(), batch); err != nil {
		log.Printf("langfuse ingestion failed request_id=%s err=%v", payload.Metadata.RequestID, err)
		http.Error(w, "upstream ingestion failed", http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("ok"))
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
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(msg))
	}

	var parsed ingestionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if len(parsed.Errors) > 0 {
		return fmt.Errorf("ingestion errors: %+v", parsed.Errors)
	}

	return nil
}

func buildLangfuseEvents(p aperturePayload, cfg config) []any {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	traceID := pick(p.Metadata.RequestID, newID())
	obsID := pick(p.Metadata.RequestID+"-gen", newID())

	est := p.EstimatedCost
	if est == nil {
		est = p.Metadata.EstimatedCost
	}

	traceMeta := map[string]any{
		"provider":       p.Metadata.Provider,
		"url":            p.Metadata.URL,
		"tailnet_name":   p.Metadata.TailnetName,
		"stable_node_id": p.Metadata.StableNodeID,
		"user_agent":     p.Metadata.UserAgent,
		"raw_responses":  rawToAny(p.RawResponses),
		"tool_calls":     rawToAny(p.ToolCalls),
	}

	traceBody := map[string]any{
		"id":          traceID,
		"timestamp":   now,
		"name":        pickNonEmpty("aperture/"+p.Metadata.Provider+"/"+p.Metadata.Model, "aperture/request"),
		"userId":      p.Metadata.LoginName,
		"sessionId":   p.Metadata.SessionID,
		"input":       pickAny(p.UserMessage, rawToAny(p.RequestBody)),
		"output":      rawToAny(p.ResponseBody),
		"metadata":    traceMeta,
		"environment": cfg.LangfuseEnv,
	}

	genBody := map[string]any{
		"id":          obsID,
		"traceId":     traceID,
		"name":        "aperture-generation",
		"startTime":   now,
		"endTime":     now,
		"model":       p.Metadata.Model,
		"input":       pickAny(p.UserMessage, rawToAny(p.RequestBody)),
		"output":      rawToAny(p.ResponseBody),
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

func (s *server) authorized(r *http.Request) bool {
	if s.cfg.ApertureAPIKey == "" {
		return true
	}
	if v := r.Header.Get("x-api-key"); v == s.cfg.ApertureAPIKey {
		return true
	}
	if v := r.Header.Get("x-goog-api-key"); v == s.cfg.ApertureAPIKey {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:]) == s.cfg.ApertureAPIKey
	}
	return auth == s.cfg.ApertureAPIKey
}

func loadConfig() (config, error) {
	cfg := config{
		TSNetEnabled:       readBool("TSNET_ENABLED", true),
		TSNetHostname:      pick(os.Getenv("TSNET_HOSTNAME"), "aperture-langfuse-relay"),
		TSNetStateDir:      pick(os.Getenv("TSNET_STATE_DIR"), "./.tsnet"),
		TSAuthKey:          os.Getenv("TS_AUTHKEY"),
		ListenAddr:         pick(os.Getenv("LISTEN_ADDR"), ":8080"),
		WebhookPath:        pick(os.Getenv("WEBHOOK_PATH"), "/hooks/aperture"),
		ApertureAPIKey:     os.Getenv("APERTURE_API_KEY"),
		LangfuseBaseURL:    pick(os.Getenv("LANGFUSE_BASE_URL"), "https://cloud.langfuse.com"),
		LangfusePublicKey:  os.Getenv("LANGFUSE_PUBLIC_KEY"),
		LangfuseSecretKey:  os.Getenv("LANGFUSE_SECRET_KEY"),
		LangfuseEnv:        pick(os.Getenv("LANGFUSE_ENV"), "production"),
		RequestTimeout:     readDuration("REQUEST_TIMEOUT", 15*time.Second),
		ShutdownTimeout:    readDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
		MaxRequestBodySize: int64(readInt("MAX_REQUEST_BODY_BYTES", 4*1024*1024)),
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
	if _, err := rand.Read(b); err != nil {
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
