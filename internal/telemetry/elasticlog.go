package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogLevel represents severity level.
type LogLevel string

const (
	LevelDebug LogLevel = "DEBUG"
	LevelInfo  LogLevel = "INFO"
	LevelWarn  LogLevel = "WARN"
	LevelError LogLevel = "ERROR"
)

// Entry defines an ECS-compliant structured log event.
type Entry struct {
	Timestamp      time.Time      `json:"@timestamp"`
	LogLevel       string         `json:"log.level"`
	ServiceName    string         `json:"service.name"`
	ServiceVersion string         `json:"service.version,omitempty"`
	HostName       string         `json:"host.name"`
	HostOS         string         `json:"host.os"`
	Message        string         `json:"message"`
	ErrorMessage   string         `json:"error.message,omitempty"`
	ProcessPID     int            `json:"process.pid"`
	JobID          string         `json:"job.id,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// ElasticConfig controls Elasticsearch / Telemetry Gateway log sink behavior.
type ElasticConfig struct {
	URL           string
	GatewayURL    string
	IsGateway     bool
	AuthToken     string
	Index         string
	Username      string
	Password      string
	ServiceName   string
	Version       string
	HostName      string
	BufferSize    int
	BatchSize     int
	FlushInterval time.Duration
	HTTPClient    *http.Client
	SpoolDir      string
}

// ElasticSink is an asynchronous, non-blocking log shipper with offline spooling.
type ElasticSink struct {
	cfg                 ElasticConfig
	queue               chan *Entry
	wg                  sync.WaitGroup
	mu                  sync.RWMutex
	closed              bool
	droppedCount        atomic.Uint64
	hostOS              string
	pid                 int
	client              *http.Client
	spoolDir            string
	spoolMu             sync.Mutex
	isDown              atomic.Bool
	consecutiveFailures atomic.Int32
	lastFailureTime     atomic.Int64
}

var (
	globalSinkMu sync.RWMutex
	globalSink   *ElasticSink
)

// SetGlobalSink sets the active global log sink.
func SetGlobalSink(s *ElasticSink) {
	globalSinkMu.Lock()
	defer globalSinkMu.Unlock()
	globalSink = s
}

// GlobalSink returns the active global log sink.
func GlobalSink() *ElasticSink {
	globalSinkMu.RLock()
	defer globalSinkMu.RUnlock()
	return globalSink
}

// EmitLog writes a log event to the global sink if initialized.
func EmitLog(level LogLevel, msg string, err error, jobID string, extra map[string]any) {
	if s := GlobalSink(); s != nil {
		s.Log(level, msg, err, jobID, extra)
	}
}

// NewElasticSink creates and starts an asynchronous log sink.
func NewElasticSink(cfg ElasticConfig) *ElasticSink {
	if cfg.Index == "" {
		cfg.Index = "iazio-logs"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "iazio-harness"
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 2048
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.HostName == "" {
		h, err := os.Hostname()
		if err == nil {
			cfg.HostName = h
		} else {
			cfg.HostName = "unknown-host"
		}
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	spoolDir := cfg.SpoolDir
	if spoolDir == "" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			spoolDir = filepath.Join(home, ".iazio", "elastic-spool")
		} else {
			spoolDir = filepath.Join(os.TempDir(), "iazio-elastic-spool")
		}
	}
	_ = os.MkdirAll(spoolDir, 0755)

	s := &ElasticSink{
		cfg:      cfg,
		queue:    make(chan *Entry, cfg.BufferSize),
		hostOS:   runtime.GOOS,
		pid:      os.Getpid(),
		client:   httpClient,
		spoolDir: spoolDir,
	}

	if strings.TrimSpace(cfg.URL) != "" || strings.TrimSpace(cfg.GatewayURL) != "" {
		s.wg.Add(1)
		go s.worker()
	}

	return s
}

// Log enqueues an Entry into the channel. Drops cleanly when full.
func (s *ElasticSink) Log(level LogLevel, msg string, err error, jobID string, extra map[string]any) {
	if s == nil {
		return
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}

	e := &Entry{
		Timestamp:      time.Now().UTC(),
		LogLevel:       string(level),
		ServiceName:    s.cfg.ServiceName,
		ServiceVersion: s.cfg.Version,
		HostName:       s.cfg.HostName,
		HostOS:         s.hostOS,
		Message:        msg,
		ErrorMessage:   errMsg,
		ProcessPID:     s.pid,
		JobID:          jobID,
		Fields:         extra,
	}

	select {
	case s.queue <- e:
	default:
		s.droppedCount.Add(1)
	}
}

// DroppedCount reports the number of logs dropped due to queue congestion.
func (s *ElasticSink) DroppedCount() uint64 {
	if s == nil {
		return 0
	}
	return s.droppedCount.Load()
}

// Close flushes the queue and stops the worker cleanly.
func (s *ElasticSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	s.wg.Wait()
	return nil
}

func (s *ElasticSink) worker() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	var batch []*Entry

	flush := func() {
		if len(batch) == 0 {
			return
		}
		toSend := batch
		batch = nil
		_ = s.sendBulk(context.Background(), toSend)
	}

	for {
		select {
		case entry, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= s.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *ElasticSink) sendBulk(ctx context.Context, entries []*Entry) error {
	dest := s.cfg.GatewayURL
	if dest == "" {
		dest = s.cfg.URL
	}
	if len(entries) == 0 || strings.TrimSpace(dest) == "" {
		return nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	if s.isDown.Load() {
		fails := s.consecutiveFailures.Load()
		backoffSec := 2 << minInt32(fails, 5) // max 64s
		lastFail := s.lastFailureTime.Load()
		if time.Now().Unix()-lastFail < int64(backoffSec) {
			s.spoolBatch(entries)
			return errors.New("telemetry gateway down; entries spooled to disk")
		}
	}

	s.drainSpool(ctx)

	err := s.sendBulkDirect(ctx, entries)
	if err != nil {
		s.isDown.Store(true)
		s.consecutiveFailures.Add(1)
		s.lastFailureTime.Store(time.Now().Unix())
		s.spoolBatch(entries)
		return err
	}

	s.isDown.Store(false)
	s.consecutiveFailures.Store(0)
	return nil
}

func (s *ElasticSink) sendBulkDirect(ctx context.Context, entries []*Entry) error {
	isGateway := s.cfg.IsGateway || s.cfg.GatewayURL != "" || strings.Contains(s.cfg.URL, "central-telemetry-gateway") || strings.Contains(s.cfg.URL, "/telemetry/logs")
	var reqURL string
	var reqBody io.Reader
	var contentType string

	if isGateway {
		gw := s.cfg.GatewayURL
		if gw == "" {
			gw = s.cfg.URL
		}
		gw = strings.TrimRight(gw, "/")
		if !strings.HasSuffix(gw, "/v1/telemetry/logs") {
			reqURL = gw + "/v1/telemetry/logs"
		} else {
			reqURL = gw
		}
		data, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
		contentType = "application/json"
	} else {
		reqURL = strings.TrimRight(s.cfg.URL, "/") + "/_bulk"
		var buf bytes.Buffer
		for _, e := range entries {
			meta := map[string]any{
				"index": map[string]any{
					"_index": s.cfg.Index,
				},
			}
			metaBytes, err := json.Marshal(meta)
			if err != nil {
				continue
			}
			entryBytes, err := json.Marshal(e)
			if err != nil {
				continue
			}
			buf.Write(metaBytes)
			buf.WriteByte('\n')
			buf.Write(entryBytes)
			buf.WriteByte('\n')
		}
		reqBody = &buf
		contentType = "application/x-ndjson"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if s.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.AuthToken)
	} else if s.cfg.Username != "" || s.cfg.Password != "" {
		req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telemetry error: status %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (s *ElasticSink) spoolBatch(entries []*Entry) {
	if len(entries) == 0 || s.spoolDir == "" {
		return
	}
	s.spoolMu.Lock()
	defer s.spoolMu.Unlock()

	spoolPath := filepath.Join(s.spoolDir, fmt.Sprintf("batch-%d.jsonl", time.Now().UnixNano()))
	f, err := os.Create(spoolPath)
	if err != nil {
		return
	}
	defer f.Close()

	for _, e := range entries {
		if data, err := json.Marshal(e); err == nil {
			f.Write(data)
			f.WriteString("\n")
		}
	}
}

func (s *ElasticSink) drainSpool(ctx context.Context) {
	if s.spoolDir == "" {
		return
	}
	s.spoolMu.Lock()
	defer s.spoolMu.Unlock()

	entries, err := os.ReadDir(s.spoolDir)
	if err != nil || len(entries) == 0 {
		return
	}

	for _, de := range entries {
		if !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(s.spoolDir, de.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var batch []*Entry
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e Entry
			if err := json.Unmarshal([]byte(line), &e); err == nil {
				batch = append(batch, &e)
			}
		}
		if len(batch) > 0 {
			if err := s.sendBulkDirect(ctx, batch); err != nil {
				return
			}
		}
		_ = os.Remove(p)
	}
}

func minInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

// ResolveTelemetryDestination discovers the central telemetry gateway URL.
// Precedence:
// 1. Env vars (CENTRAL_TELEMETRY_GATEWAY_URL, IAZIO_CENTRAL_TELEMETRY_GATEWAY_URL)
// 2. Upstream API endpoint (/v1/telemetry/config, /v1/observability/metrics, /v1/status)
// 3. Config file (~/.iazio/harness.json, ~/.iazio/agent.json)
// 4. IAZIO_ELASTICSEARCH_URL fallback
func ResolveTelemetryDestination(apiURL string, getenv func(string) string) (string, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, envKey := range []string{"CENTRAL_TELEMETRY_GATEWAY_URL", "IAZIO_CENTRAL_TELEMETRY_GATEWAY_URL", "TELEMETRY_GATEWAY_URL"} {
		if u := strings.TrimSpace(getenv(envKey)); u != "" {
			return u, true
		}
	}

	if trimmedAPI := strings.TrimRight(strings.TrimSpace(apiURL), "/"); trimmedAPI != "" {
		client := &http.Client{Timeout: 2 * time.Second}
		for _, path := range []string{"/v1/telemetry/config", "/api/v1/telemetry/config", "/v1/observability/metrics", "/v1/status"} {
			resp, err := client.Get(trimmedAPI + path)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var raw struct {
						CentralTelemetryGatewayURL string `json:"central_telemetry_gateway_url"`
						Telemetry                  struct {
							CentralTelemetryGatewayURL string `json:"central_telemetry_gateway_url"`
						} `json:"telemetry"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&raw); err == nil {
						if strings.TrimSpace(raw.CentralTelemetryGatewayURL) != "" {
							return strings.TrimSpace(raw.CentralTelemetryGatewayURL), true
						}
						if strings.TrimSpace(raw.Telemetry.CentralTelemetryGatewayURL) != "" {
							return strings.TrimSpace(raw.Telemetry.CentralTelemetryGatewayURL), true
						}
					}
				}
			}
		}
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, cfgName := range []string{"harness.json", "agent.json"} {
			cfgFile := filepath.Join(home, ".iazio", cfgName)
			if data, err := os.ReadFile(cfgFile); err == nil {
				var raw struct {
					CentralTelemetryGatewayURL string `json:"central_telemetry_gateway_url"`
					ElasticsearchURL           string `json:"elasticsearch_url"`
				}
				if err := json.Unmarshal(data, &raw); err == nil {
					if strings.TrimSpace(raw.CentralTelemetryGatewayURL) != "" {
						return strings.TrimSpace(raw.CentralTelemetryGatewayURL), true
					}
					if strings.TrimSpace(raw.ElasticsearchURL) != "" {
						return strings.TrimSpace(raw.ElasticsearchURL), false
					}
				}
			}
		}
	}

	if esURL := strings.TrimSpace(getenv("IAZIO_ELASTICSEARCH_URL")); esURL != "" {
		return esURL, false
	}

	return "", false
}

// SlogHandler wraps an existing slog.Handler and mirrors log events to an ElasticSink.
type SlogHandler struct {
	sink *ElasticSink
	next slog.Handler
}

// NewSlogHandler creates a slog.Handler forwarding events to both next and sink.
// If next is nil or is Go's internal *slog.defaultHandler (which writes back to
// log.Default() and deadlocks when slog.SetDefault redirects log.Printf into slog),
// it falls back to a standard stderr TextHandler.
func NewSlogHandler(sink *ElasticSink, next slog.Handler) *SlogHandler {
	if next == nil || fmt.Sprintf("%T", next) == "*slog.defaultHandler" {
		next = slog.NewTextHandler(os.Stderr, nil)
	}
	return &SlogHandler{
		sink: sink,
		next: next,
	}
}

func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.next != nil {
		return h.next.Enabled(ctx, level)
	}
	return true
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	var nextErr error
	if h.next != nil {
		nextErr = h.next.Handle(ctx, r)
	}
	if h.sink != nil {
		fields := make(map[string]any)
		r.Attrs(func(a slog.Attr) bool {
			fields[a.Key] = a.Value.Any()
			return true
		})
		lvl := LevelInfo
		switch {
		case r.Level >= slog.LevelError:
			lvl = LevelError
		case r.Level >= slog.LevelWarn:
			lvl = LevelWarn
		case r.Level < slog.LevelInfo:
			lvl = LevelDebug
		}
		var errVal error
		if errAttr, ok := fields["error"]; ok {
			if e, ok := errAttr.(error); ok {
				errVal = e
			} else if s, ok := errAttr.(string); ok {
				errVal = errors.New(s)
			}
		} else if errAttr, ok := fields["err"]; ok {
			if e, ok := errAttr.(error); ok {
				errVal = e
			} else if s, ok := errAttr.(string); ok {
				errVal = errors.New(s)
			}
		}
		h.sink.Log(lvl, r.Message, errVal, "", fields)
	}
	return nextErr
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithAttrs(attrs)
	}
	return &SlogHandler{sink: h.sink, next: next}
}

func (h *SlogHandler) WithGroup(name string) slog.Handler {
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithGroup(name)
	}
	return &SlogHandler{sink: h.sink, next: next}
}
