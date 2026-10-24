package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGateBeforeCLI(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_CONFIG", t.TempDir()+"/missing.json")
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "1")
	t.Setenv("INVOCATION_ID", "")
	if err := GateBeforeCLI(); err == nil {
		t.Fatal("expected missing refresh token")
	}

	// Interactive mode without refresh token
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "")
	if err := GateBeforeCLI(); err == nil || !strings.Contains(err.Error(), "run auth login") {
		t.Fatalf("expected run auth login hint, got %v", err)
	}

	// With refresh token present
	cfgFile := filepath.Join(t.TempDir(), "harness.json")
	_ = os.WriteFile(cfgFile, []byte(`{"refresh_token": "present-token"}`), 0o600)
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgFile)
	if err := GateBeforeCLI(); err != nil {
		t.Fatalf("expected nil error when token present, got %v", err)
	}
}

func TestReportString(t *testing.T) {
	r1 := Report{ConfigPath: "/tmp/cfg.json", RefreshPresent: false}
	if !strings.Contains(r1.String(), "absent") {
		t.Errorf("expected absent in report string, got %s", r1.String())
	}
	r2 := Report{ConfigPath: "/tmp/cfg.json", RefreshPresent: true}
	if !strings.Contains(r2.String(), "present") {
		t.Errorf("expected present in report string, got %s", r2.String())
	}
}

func TestConfigPath(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_CONFIG", "/custom/path/harness.json")
	if ConfigPath() != "/custom/path/harness.json" {
		t.Errorf("expected custom path, got %s", ConfigPath())
	}
	t.Setenv("IAZIO_HARNESS_CONFIG", "")
	p := ConfigPath()
	if !strings.HasSuffix(p, ".iazio/harness.json") {
		t.Errorf("expected default suffix, got %s", p)
	}
}

func TestIsNonInteractive(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "1")
	t.Setenv("INVOCATION_ID", "")
	if !IsNonInteractive() {
		t.Errorf("expected true when noninteractive=1")
	}

	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "0")
	t.Setenv("INVOCATION_ID", "inv-123")
	if !IsNonInteractive() {
		t.Errorf("expected true when invocation_id set")
	}

	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "")
	t.Setenv("INVOCATION_ID", "")
	if IsNonInteractive() {
		t.Errorf("expected false when neither set")
	}
}

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	missingFile := filepath.Join(dir, "missing.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", missingFile)

	st, err := Status()
	if err != nil || st.RefreshPresent {
		t.Errorf("expected absent on missing file, got st=%+v, err=%v", st, err)
	}

	badFile := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(badFile, []byte("{invalid"), 0o600)
	t.Setenv("IAZIO_HARNESS_CONFIG", badFile)
	_, err = Status()
	if err == nil {
		t.Errorf("expected error on invalid json")
	}

	goodFile := filepath.Join(dir, "good.json")
	_ = os.WriteFile(goodFile, []byte(`{"refresh_token":"rt-999"}`), 0o600)
	t.Setenv("IAZIO_HARNESS_CONFIG", goodFile)
	st, err = Status()
	if err != nil || !st.RefreshPresent {
		t.Errorf("expected present, got st=%+v, err=%v", st, err)
	}
}

func TestResolveEndpoints(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_CONFIG", t.TempDir()+"/none.json")
	t.Setenv("IAZIO_AUTH_URL", "https://auth.example.com")
	t.Setenv("IAZIO_TOKEN_URL", "https://token.example.com")
	t.Setenv("IAZIO_DEVICE_AUTH_URL", "https://device.example.com")

	ep, err := ResolveEndpoints()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ep.AuthURL != "https://auth.example.com" || ep.TokenURL != "https://token.example.com" || ep.DeviceAuthURL != "https://device.example.com" {
		t.Errorf("endpoints mismatch: %+v", ep)
	}
}

func TestBearerAndRefresh(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "auth.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgPath)

	// 1. Usable token already present
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	initialCfg := &File{
		AccessToken:  "valid-at",
		RefreshToken: "valid-rt",
		Expiry:       future,
	}
	_ = save(cfgPath, initialCfg)

	ctx := context.Background()
	tok, err := Bearer(ctx)
	if err != nil || tok != "valid-at" {
		t.Fatalf("expected valid-at, got tok=%s, err=%v", tok, err)
	}

	// 2. Token expired, requires refresh
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	expiredCfg := &File{
		AccessToken:  "expired-at",
		RefreshToken: "valid-rt",
		Expiry:       past,
	}
	_ = save(cfgPath, expiredCfg)

	// Mock token server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "refreshed-at",
			RefreshToken: "refreshed-rt",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer ts.Close()

	t.Setenv("IAZIO_TOKEN_URL", ts.URL+"/token")
	tok, err = Bearer(ctx)
	if err != nil || tok != "refreshed-at" {
		t.Fatalf("expected refreshed-at, got tok=%s, err=%v", tok, err)
	}

	// 3. No refresh token in file
	emptyCfg := &File{}
	_ = save(cfgPath, emptyCfg)
	_, err = Bearer(ctx)
	if err != ErrNoRefreshToken {
		t.Fatalf("expected ErrNoRefreshToken, got %v", err)
	}
}

func TestLoginFlows(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "auth.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgPath)
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "1")

	// Noninteractive without token fails
	ctx := context.Background()
	err := Login(ctx)
	if err == nil {
		t.Fatalf("expected noninteractive login error")
	}

	// Noninteractive with valid token succeeds
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	_ = save(cfgPath, &File{
		AccessToken:  "existing-at",
		RefreshToken: "existing-rt",
		Expiry:       future,
	})
	if err := Login(ctx); err != nil {
		t.Fatalf("expected login success with existing token, got %v", err)
	}

	// Interactive with refresh present succeeds
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "")
	if err := Login(ctx); err != nil {
		t.Fatalf("expected interactive login success with existing token, got %v", err)
	}

	// Interactive with missing token URL fails
	_ = os.Remove(cfgPath)
	t.Setenv("IAZIO_TOKEN_URL", "")
	t.Setenv("IAZIO_AUTH_URL", "")
	err = Login(ctx)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected token endpoint error, got %v", err)
	}
}

func TestPKCEAndRandom(t *testing.T) {
	verifier, challenge, err := newPKCE()
	if err != nil || verifier == "" || challenge == "" {
		t.Fatalf("newPKCE failed: v=%s, c=%s, err=%v", verifier, challenge, err)
	}
	rnd, err := randomString(20)
	if err != nil || len(rnd) == 0 {
		t.Fatalf("randomString failed: %s, err=%v", rnd, err)
	}
}

func TestSaveErrors(t *testing.T) {
	if err := save("/some/path", nil); err == nil {
		t.Errorf("expected error for nil config")
	}
}

func TestStoreFromForm(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "from-form-at",
			RefreshToken: "from-form-rt",
			ExpiresIn:    1800,
		})
	}))
	defer ts.Close()

	cfgPath := filepath.Join(t.TempDir(), "harness.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgPath)

	ep := Endpoints{
		TokenURL: ts.URL + "/token",
		AuthURL:  "https://auth.example.com",
	}
	form := url.Values{"code": {"auth-code"}}
	err := storeFromForm(context.Background(), ep, form)
	if err != nil {
		t.Fatalf("storeFromForm failed: %v", err)
	}

	cfg, _ := load(cfgPath)
	if cfg.AccessToken != "from-form-at" {
		t.Errorf("expected from-form-at, got %s", cfg.AccessToken)
	}
}

func TestAuthorizeURL(t *testing.T) {
	// Missing AuthURL
	_, err := AuthorizeURL(Endpoints{}, "http://127.0.0.1/cb", "st1", "ch1")
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Errorf("expected auth endpoint required, got %v", err)
	}

	// Valid AuthURL
	u, err := AuthorizeURL(Endpoints{AuthURL: "https://auth.example.com/oauth/authorize"}, "http://127.0.0.1/cb", "st1", "ch1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(u, "code_challenge=ch1") || !strings.Contains(u, "state=st1") {
		t.Errorf("expected query params in URL: %s", u)
	}
}

func TestDeviceLogin(t *testing.T) {
	pollCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-code-123",
				"user_code":        "ABCD",
				"verification_uri": "https://auth.example.com/verify",
				"interval":         1,
				"expires_in":       10,
			})
			return
		}
		if r.URL.Path == "/token" {
			pollCount++
			if pollCount < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(tokenResponse{
				AccessToken:  "dev-flow-at",
				RefreshToken: "dev-flow-rt",
				ExpiresIn:    3600,
			})
			return
		}
	}))
	defer ts.Close()

	cfgPath := filepath.Join(t.TempDir(), "harness.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgPath)

	ep := Endpoints{
		DeviceAuthURL: ts.URL + "/device",
		TokenURL:      ts.URL + "/token",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := deviceLogin(ctx, ep)
	if err != nil {
		t.Fatalf("deviceLogin failed: %v", err)
	}

	cfg, _ := load(cfgPath)
	if cfg.AccessToken != "dev-flow-at" {
		t.Errorf("expected dev-flow-at, got %s", cfg.AccessToken)
	}

	// Interactive login selecting device flow when AuthURL is empty
	err = interactiveLogin(ctx)
	// ep without token url fails
	t.Setenv("IAZIO_TOKEN_URL", "")
	t.Setenv("IAZIO_DEVICE_AUTH_URL", "")
	err = interactiveLogin(ctx)
	if err == nil {
		t.Errorf("expected error when endpoints missing")
	}
}

func TestLoopbackLogin_Cancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	ep := Endpoints{
		AuthURL:  "https://auth.example.com/oauth/authorize",
		TokenURL: "https://auth.example.com/oauth/token",
	}
	err := loopbackLogin(ctx, ep)
	if err == nil {
		t.Errorf("expected context canceled error")
	}
}

func TestAuthHelpers_Comprehensive(t *testing.T) {
	// 1. randomString and newPKCE
	rStr, err := randomString(16)
	if err != nil || len(rStr) == 0 {
		t.Fatalf("expected valid randomString, got %s, %v", rStr, err)
	}
	verifier, challenge, err := newPKCE()
	if err != nil || verifier == "" || challenge == "" {
		t.Fatalf("expected valid PKCE, got verifier=%s, challenge=%s, err=%v", verifier, challenge, err)
	}

	// 2. dirOf
	if d := dirOf("/foo/bar/baz.json"); d != "/foo/bar" {
		t.Errorf("expected /foo/bar, got %s", d)
	}
	if d := dirOf("file.json"); d != "" {
		t.Errorf("expected empty string for relative filename, got %s", d)
	}

	// 3. accessUsable
	if accessUsable(nil) {
		t.Errorf("expected false for nil cfg")
	}
	if accessUsable(&File{AccessToken: ""}) {
		t.Errorf("expected false for empty token")
	}
	if accessUsable(&File{AccessToken: "tok", Expiry: ""}) {
		t.Errorf("expected false for empty expiry")
	}
	if accessUsable(&File{AccessToken: "tok", Expiry: "not-a-date"}) {
		t.Errorf("expected false for invalid expiry format")
	}
	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	if accessUsable(&File{AccessToken: "tok", Expiry: past}) {
		t.Errorf("expected false for past expiry")
	}
	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	if !accessUsable(&File{AccessToken: "tok", Expiry: future}) {
		t.Errorf("expected true for future expiry")
	}

	// 4. applyToken
	cfg := &File{}
	applyToken(cfg, tokenResponse{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		TokenType:    "Bearer",
		ExpiresIn:    0, // defaults to 1800
	})
	if cfg.AccessToken != "at-1" || cfg.RefreshToken != "rt-1" || cfg.TokenType != "Bearer" || cfg.Expiry == "" {
		t.Errorf("applyToken failed to populate fields: %+v", cfg)
	}

	// 5. postForm error conditions
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/err-500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/err-json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("invalid json"))
		case "/err-field":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(tokenResponse{Error: "invalid_grant"})
		case "/ok":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(tokenResponse{
				AccessToken:  "ok-token",
				RefreshToken: "ok-refresh",
				ExpiresIn:    1800,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	ctx := context.Background()
	vals := url.Values{"grant_type": []string{"authorization_code"}}

	if _, err := postForm(ctx, ts.URL+"/err-500", vals); err == nil {
		t.Errorf("expected error on 500")
	}
	if _, err := postForm(ctx, ts.URL+"/err-json", vals); err == nil {
		t.Errorf("expected error on invalid json")
	}
	if _, err := postForm(ctx, ts.URL+"/err-field", vals); err == nil {
		t.Errorf("expected error on token response with error")
	}
	tok, err := postForm(ctx, ts.URL+"/ok", vals)
	if err != nil || tok.AccessToken != "ok-token" {
		t.Errorf("expected successful postForm, got %+v, err=%v", tok, err)
	}

	// 6. storeFromForm
	cfgFile := filepath.Join(t.TempDir(), "store_test.json")
	t.Setenv("IAZIO_HARNESS_CONFIG", cfgFile)
	err = storeFromForm(ctx, Endpoints{
		AuthURL:  "https://auth.example.com/oauth/authorize",
		TokenURL: ts.URL + "/ok",
	}, vals)
	if err != nil {
		t.Fatalf("storeFromForm failed: %v", err)
	}
	savedCfg, err := loadOptional()
	if err != nil || savedCfg.AccessToken != "ok-token" {
		t.Fatalf("expected savedCfg token ok-token, got %+v, err=%v", savedCfg, err)
	}

	// 7. refreshPresent
	pres, err := refreshPresent()
	if err != nil || !pres {
		t.Errorf("expected refreshPresent=true, got %v, %v", pres, err)
	}
}
