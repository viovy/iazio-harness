// Package sandbox provides deterministic mock proxying and workspace snapshotting for hermetic agent evaluations.
package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProxyMode defines whether the proxy records or replays calls.
type ProxyMode string

const (
	// ProxyModeRecord calls upstream and saves interactions to cassette.
	ProxyModeRecord ProxyMode = "record"
	// ProxyModeReplay serves responses strictly from cassette without network egress.
	ProxyModeReplay ProxyMode = "replay"
	// ProxyModePassthrough proxies requests directly without saving.
	ProxyModePassthrough ProxyMode = "passthrough"
)

// RecordedInteraction is one HTTP/MCP interaction saved in a cassette.
type RecordedInteraction struct {
	Method          string              `json:"method"`
	URL             string              `json:"url"`
	RequestBodyHash string              `json:"request_body_hash"`
	RequestBody     string              `json:"request_body,omitempty"`
	StatusCode      int                 `json:"status_code"`
	ResponseBody    string              `json:"response_body"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	Timestamp       time.Time           `json:"timestamp"`
}

// Cassette stores a sequence of recorded interactions.
type Cassette struct {
	Name         string                `json:"name"`
	Interactions []RecordedInteraction `json:"interactions"`
}

// RecordReplayProxy is an HTTP handler that records or replays requests deterministically.
type RecordReplayProxy struct {
	mode         ProxyMode
	cassettePath string
	cassette     Cassette
	upstreamURL  string
	mu           sync.Mutex
	client       *http.Client
}

// NewRecordReplayProxy creates a hermetic mock proxy.
func NewRecordReplayProxy(mode ProxyMode, cassettePath, upstreamURL string) (*RecordReplayProxy, error) {
	if mode == "" {
		mode = ProxyModeReplay
	}

	p := &RecordReplayProxy{
		mode:         mode,
		cassettePath: cassettePath,
		upstreamURL:  strings.TrimRight(upstreamURL, "/"),
		client:       &http.Client{Timeout: 30 * time.Second},
		cassette:     Cassette{Name: filepath.Base(cassettePath)},
	}

	if (mode == ProxyModeReplay || mode == ProxyModeRecord) && cassettePath != "" {
		_ = p.LoadCassette()
	}

	return p, nil
}

// ServeHTTP handles incoming mock/upstream requests.
func (p *RecordReplayProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body error", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	bodyHash := hashBytes(bodyBytes)
	reqURL := r.URL.String()

	switch p.mode {
	case ProxyModeReplay:
		p.mu.Lock()
		match := p.findMatch(r.Method, reqURL, bodyHash)
		p.mu.Unlock()

		if match == nil {
			http.Error(w, fmt.Sprintf("no matching interaction in cassette for %s %s (hash %s)", r.Method, reqURL, bodyHash), http.StatusServiceUnavailable)
			return
		}

		for k, v := range match.ResponseHeaders {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
		w.WriteHeader(match.StatusCode)
		_, _ = w.Write([]byte(match.ResponseBody))

	case ProxyModeRecord:
		// Forward to upstream
		resp, err := p.forwardToUpstream(r, bodyBytes)
		if err != nil {
			http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		respBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "read upstream response error", http.StatusInternalServerError)
			return
		}

		interaction := RecordedInteraction{
			Method:          r.Method,
			URL:             reqURL,
			RequestBodyHash: bodyHash,
			RequestBody:     string(bodyBytes),
			StatusCode:      resp.StatusCode,
			ResponseBody:    string(respBytes),
			ResponseHeaders: resp.Header.Clone(),
			Timestamp:       time.Now().UTC(),
		}

		p.mu.Lock()
		p.cassette.Interactions = append(p.cassette.Interactions, interaction)
		_ = p.saveCassetteLocked()
		p.mu.Unlock()

		for k, v := range resp.Header {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBytes)

	case ProxyModePassthrough:
		resp, err := p.forwardToUpstream(r, bodyBytes)
		if err != nil {
			http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, v := range resp.Header {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func (p *RecordReplayProxy) findMatch(method, reqURL, bodyHash string) *RecordedInteraction {
	for _, inter := range p.cassette.Interactions {
		if inter.Method == method && (inter.URL == reqURL || strings.HasSuffix(reqURL, inter.URL)) && (inter.RequestBodyHash == bodyHash || inter.RequestBodyHash == "") {
			return &inter
		}
	}
	// Fallback to match by URL prefix and method
	for _, inter := range p.cassette.Interactions {
		if inter.Method == method && (inter.URL == reqURL || strings.HasSuffix(reqURL, inter.URL)) {
			return &inter
		}
	}
	return nil
}

func (p *RecordReplayProxy) forwardToUpstream(r *http.Request, bodyBytes []byte) (*http.Response, error) {
	if p.upstreamURL == "" {
		return nil, fmt.Errorf("upstream URL is not configured")
	}

	targetURL := p.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(r.Method, parsed.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	for k, v := range r.Header {
		req.Header[k] = v
	}

	return p.client.Do(req)
}

// LoadCassette reads the cassette JSON file from disk.
func (p *RecordReplayProxy) LoadCassette() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cassettePath == "" {
		return nil
	}

	data, err := os.ReadFile(p.cassettePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	return json.Unmarshal(data, &p.cassette)
}

func (p *RecordReplayProxy) saveCassetteLocked() error {
	if p.cassettePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(p.cassette, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p.cassettePath), 0755); err != nil {
		return err
	}

	return os.WriteFile(p.cassettePath, data, 0644)
}

// AddInteraction manually adds an interaction to the cassette (for programmatic tests).
func (p *RecordReplayProxy) AddInteraction(interaction RecordedInteraction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cassette.Interactions = append(p.cassette.Interactions, interaction)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
