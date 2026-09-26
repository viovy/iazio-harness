// Package auth stores the harness OAuth client file and refuses interactive login when none is possible.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ClientID is the public OAuth client id for this binary.
const ClientID = "iazio-harness-cli"

// ErrNoRefreshToken is returned when a run or a noninteractive login has no stored refresh token.
var ErrNoRefreshToken = errors.New("no refresh token")

// ErrEndpointRequired is returned when an OAuth URL is missing from the environment and the config file.
var ErrEndpointRequired = errors.New("oauth endpoint is not configured")

const accessSkew = 60 * time.Second

// HTTPClient is the client used for token requests. Tests may replace it.
var HTTPClient = http.DefaultClient

// File is the on-disk harness client configuration.
type File struct {
	ClientID      string `json:"client_id,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	Expiry        string `json:"expiry,omitempty"`
	AuthURL       string `json:"auth_url,omitempty"`
	TokenURL      string `json:"token_url,omitempty"`
	DeviceAuthURL string `json:"device_auth_url,omitempty"`
}

// Endpoints are the OAuth URLs for this process.
type Endpoints struct {
	AuthURL       string
	TokenURL      string
	DeviceAuthURL string
}

// Report is the redacted, offline auth status.
type Report struct {
	ConfigPath     string
	RefreshPresent bool
}

// String prints the config path and whether a refresh token is present.
// It never includes token material.
func (r Report) String() string {
	state := "absent"
	if r.RefreshPresent {
		state = "present"
	}
	return fmt.Sprintf("config: %s\nrefresh_token: %s\n", r.ConfigPath, state)
}

// ConfigPath returns IAZIO_HARNESS_CONFIG or ~/.iazio/harness.json.
func ConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("IAZIO_HARNESS_CONFIG")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".iazio/harness.json"
	}
	return home + "/.iazio/harness.json"
}

// IsNonInteractive reports whether this process must not open a browser or device prompt.
// IAZIO_HARNESS_NONINTERACTIVE=1 or a non-empty INVOCATION_ID selects that mode.
func IsNonInteractive() bool {
	if strings.TrimSpace(os.Getenv("IAZIO_HARNESS_NONINTERACTIVE")) == "1" {
		return true
	}
	return strings.TrimSpace(os.Getenv("INVOCATION_ID")) != ""
}

// GateBeforeCLI fails before an IDE CLI starts when this process has no refresh token.
// Noninteractive runs fail with ErrNoRefreshToken wrapped as a noninteractive error.
func GateBeforeCLI() error {
	present, err := refreshPresent()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if IsNonInteractive() {
		return fmt.Errorf("noninteractive: %w", ErrNoRefreshToken)
	}
	return fmt.Errorf("%w: run auth login", ErrNoRefreshToken)
}

// Status reads the local file only. It does not call the network and does not return token bytes.
func Status() (Report, error) {
	path := ConfigPath()
	cfg, err := load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Report{ConfigPath: path}, nil
		}
		return Report{}, err
	}
	return Report{
		ConfigPath:     path,
		RefreshPresent: strings.TrimSpace(cfg.RefreshToken) != "",
	}, nil
}

// ResolveEndpoints returns authorize, token, and device URLs from the environment, then the config file.
func ResolveEndpoints() (Endpoints, error) {
	cfg, err := loadOptional()
	if err != nil {
		return Endpoints{}, err
	}
	ep := Endpoints{
		AuthURL:       firstNonEmpty(os.Getenv("IAZIO_AUTH_URL"), cfg.AuthURL),
		TokenURL:      firstNonEmpty(os.Getenv("IAZIO_TOKEN_URL"), cfg.TokenURL),
		DeviceAuthURL: firstNonEmpty(os.Getenv("IAZIO_DEVICE_AUTH_URL"), cfg.DeviceAuthURL),
	}
	return ep, nil
}

// Bearer returns an access token string for API calls.
// It reuses an unexpired access token, otherwise it refreshes once.
func Bearer(ctx context.Context) (string, error) {
	path := ConfigPath()
	cfg, err := loadOptional()
	if err != nil {
		return "", err
	}
	if accessUsable(cfg) {
		return cfg.AccessToken, nil
	}
	if strings.TrimSpace(cfg.RefreshToken) == "" {
		return "", ErrNoRefreshToken
	}
	return refresh(ctx, path, cfg)
}

// Login acquires a refresh token and stores it.
// Noninteractive mode fails before any browser or device prompt when no refresh token is stored.
func Login(ctx context.Context) error {
	if IsNonInteractive() {
		if err := GateBeforeCLI(); err != nil {
			return err
		}
		_, err := Bearer(ctx)
		return err
	}
	if present, err := refreshPresent(); err != nil {
		return err
	} else if present {
		_, err = Bearer(ctx)
		return err
	}
	return interactiveLogin(ctx)
}

func interactiveLogin(ctx context.Context) error {
	ep, err := ResolveEndpoints()
	if err != nil {
		return err
	}
	if ep.TokenURL == "" {
		return fmt.Errorf("%w: token", ErrEndpointRequired)
	}
	if ep.DeviceAuthURL != "" && ep.AuthURL == "" {
		return deviceLogin(ctx, ep)
	}
	if ep.AuthURL == "" {
		return fmt.Errorf("%w: auth", ErrEndpointRequired)
	}
	if err := loopbackLogin(ctx, ep); err != nil && ep.DeviceAuthURL != "" {
		return deviceLogin(ctx, ep)
	} else {
		return err
	}
}

func refreshPresent() (bool, error) {
	cfg, err := loadOptional()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(cfg.RefreshToken) != "", nil
}

func loadOptional() (*File, error) {
	cfg, err := load(ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{}, nil
		}
		return nil, err
	}
	return cfg, nil
}

func load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg File
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, errors.New("config file is not valid json")
	}
	return &cfg, nil
}

func save(path string, cfg *File) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		cfg.ClientID = ClientID
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := dirOf(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dirOf(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return ""
	}
	return path[:i]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func accessUsable(cfg *File) bool {
	if cfg == nil || strings.TrimSpace(cfg.AccessToken) == "" {
		return false
	}
	exp := strings.TrimSpace(cfg.Expiry)
	if exp == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return false
	}
	return time.Now().Add(accessSkew).Before(t)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func refresh(ctx context.Context, path string, cfg *File) (string, error) {
	ep, err := ResolveEndpoints()
	if err != nil {
		return "", err
	}
	if ep.TokenURL == "" {
		return "", fmt.Errorf("%w: token", ErrEndpointRequired)
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cfg.RefreshToken)
	form.Set("client_id", ClientID)
	tok, err := postForm(ctx, ep.TokenURL, form)
	if err != nil {
		return "", err
	}
	applyToken(cfg, tok)
	cfg.AuthURL = firstNonEmpty(cfg.AuthURL, ep.AuthURL)
	cfg.TokenURL = firstNonEmpty(cfg.TokenURL, ep.TokenURL)
	cfg.DeviceAuthURL = firstNonEmpty(cfg.DeviceAuthURL, ep.DeviceAuthURL)
	if err := save(path, cfg); err != nil {
		return "", err
	}
	return cfg.AccessToken, nil
}

func applyToken(cfg *File, tok tokenResponse) {
	if strings.TrimSpace(tok.AccessToken) != "" {
		cfg.AccessToken = tok.AccessToken
	}
	if strings.TrimSpace(tok.RefreshToken) != "" {
		cfg.RefreshToken = tok.RefreshToken
	}
	if strings.TrimSpace(tok.TokenType) != "" {
		cfg.TokenType = tok.TokenType
	}
	secs := tok.ExpiresIn
	if secs <= 0 {
		secs = 1800
	}
	cfg.Expiry = time.Now().Add(time.Duration(secs) * time.Second).UTC().Format(time.RFC3339)
}

func postForm(ctx context.Context, endpoint string, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return tokenResponse{}, errors.New("token request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, errors.New("token request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenResponse{}, fmt.Errorf("token request failed: http %d", resp.StatusCode)
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, errors.New("token request failed")
	}
	if tok.Error != "" || strings.TrimSpace(tok.AccessToken) == "" {
		return tokenResponse{}, errors.New("token request failed")
	}
	return tok, nil
}

func loopbackLogin(ctx context.Context, ep Endpoints) error {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return err
	}
	state, err := randomString(16)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	authURL, err := AuthorizeURL(ep, redirect, state, challenge)
	if err != nil {
		ln.Close()
		return err
	}
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "login failed", http.StatusBadRequest)
			errCh <- errors.New("login failed")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("missing code")
			return
		}
		fmt.Fprintln(w, "Login complete. You can close this window.")
		codeCh <- code
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "Open this URL to log in:\n%s\n", authURL)
	}
	var code string
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	case code = <-codeCh:
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", ClientID)
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	return storeFromForm(ctx, ep, form)
}

// AuthorizeURL builds the browser authorization URL from the given endpoints.
func AuthorizeURL(ep Endpoints, redirect, state, challenge string) (string, error) {
	if strings.TrimSpace(ep.AuthURL) == "" {
		return "", fmt.Errorf("%w: auth", ErrEndpointRequired)
	}
	u, err := url.Parse(ep.AuthURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", "offline_access openid profile")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func deviceLogin(ctx context.Context, ep Endpoints) error {
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("scope", "offline_access openid profile")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.DeviceAuthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return errors.New("device request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("device request failed")
	}
	var start struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &start); err != nil || start.DeviceCode == "" {
		return errors.New("device request failed")
	}
	fmt.Fprintf(os.Stderr, "Open %s and enter code %s\n", firstNonEmpty(start.VerificationURIComplete, start.VerificationURI), start.UserCode)
	interval := start.Interval
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	if start.ExpiresIn <= 0 {
		deadline = time.Now().Add(5 * time.Minute)
	}
	for {
		if time.Now().After(deadline) {
			return errors.New("device login expired")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
		poll := url.Values{}
		poll.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		poll.Set("device_code", start.DeviceCode)
		poll.Set("client_id", ClientID)
		tok, err := postForm(ctx, ep.TokenURL, poll)
		if err == nil {
			cfg, _ := loadOptional()
			if cfg == nil {
				cfg = &File{}
			}
			applyToken(cfg, tok)
			cfg.AuthURL = ep.AuthURL
			cfg.TokenURL = ep.TokenURL
			cfg.DeviceAuthURL = ep.DeviceAuthURL
			return save(ConfigPath(), cfg)
		}
	}
}

func storeFromForm(ctx context.Context, ep Endpoints, form url.Values) error {
	tok, err := postForm(ctx, ep.TokenURL, form)
	if err != nil {
		return err
	}
	cfg, err := loadOptional()
	if err != nil {
		return err
	}
	applyToken(cfg, tok)
	cfg.AuthURL = ep.AuthURL
	cfg.TokenURL = ep.TokenURL
	cfg.DeviceAuthURL = ep.DeviceAuthURL
	return save(ConfigPath(), cfg)
}

func newPKCE() (verifier, challenge string, err error) {
	verifier, err = randomString(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}
