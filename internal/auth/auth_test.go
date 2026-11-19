package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

func TestGitEnvAndRedactor(t *testing.T) {
	redactor := NewRedactor()
	t.Setenv("TEST_PAT", "ghp_supersecret123")
	tp, err := NewTokenProvider(config.AuthConfig{Mode: "pat", TokenEnv: "TEST_PAT"}, "http://user:proxypass@127.0.0.1:7890", redactor)
	if err != nil {
		t.Fatalf("NewTokenProvider failed: %v", err)
	}
	tok, err := tp.Token(context.Background())
	if err != nil || tok != "ghp_supersecret123" {
		t.Fatalf("unexpected token: %q, err=%v", tok, err)
	}

	netCfg := config.NetworkConfig{
		ProxyURL:      "http://user:proxypass@127.0.0.1:7890",
		HTTPVersion:   "HTTP/1.1",
		LowSpeedLimit: 1000,
		LowSpeedTime:  60,
	}
	env := GitEnv([]string{"PATH=/usr/bin", "GIT_CONFIG_COUNT=99"}, tok, netCfg)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=4") {
		t.Fatalf("expected GIT_CONFIG_COUNT=4, got %s", joined)
	}
	if !strings.Contains(joined, "http.version") || !strings.Contains(joined, "HTTP/1.1") || !strings.Contains(joined, "http.lowSpeedLimit") {
		t.Fatalf("missing HTTP stability git config vars: %s", joined)
	}
	if !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(joined, "HTTPS_PROXY=http://user:proxypass@127.0.0.1:7890") {
		t.Fatalf("missing expected env vars: %s", joined)
	}

	expectedB64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_supersecret123"))
	msg := "fatal: header AUTHORIZATION: basic " + expectedB64 + " via proxypass and ghp_supersecret123"
	redacted := redactor.Redact(msg)
	if strings.Contains(redacted, "ghp_supersecret123") || strings.Contains(redacted, expectedB64) || strings.Contains(redacted, "proxypass") {
		t.Fatalf("secret leaked in redacted output: %s", redacted)
	}
}

func TestGitHubAppTokenRefreshBeforeExpiry(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	baseTime := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_token_" + string(rune('0'+n)),
			"expires_at": baseTime.Add(time.Duration(n) * time.Hour),
		})
	}))
	defer srv.Close()

	tp, err := NewTokenProvider(config.AuthConfig{
		Mode:           "github_app",
		AppID:          123,
		InstallationID: 456,
		PrivateKeyPath: keyPath,
	}, "", NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	tp.SetAPIBaseURL(srv.URL)

	currentNow := baseTime
	tp.nowFunc = func() time.Time { return currentNow }

	tok1, err := tp.Token(context.Background())
	if err != nil || tok1 != "ghs_token_1" {
		t.Fatalf("expected ghs_token_1, got %q err=%v", tok1, err)
	}

	// 30m later (30m remaining > 10m margin): cached token should be reused
	currentNow = baseTime.Add(30 * time.Minute)
	tok2, err := tp.Token(context.Background())
	if err != nil || tok2 != "ghs_token_1" || calls.Load() != 1 {
		t.Fatalf("expected cached token, got %q calls=%d err=%v", tok2, calls.Load(), err)
	}

	// 52m later (8m remaining < 10m margin): should auto refresh
	currentNow = baseTime.Add(52 * time.Minute)
	tok3, err := tp.Token(context.Background())
	if err != nil || tok3 != "ghs_token_2" || calls.Load() != 2 {
		t.Fatalf("expected refreshed token, got %q calls=%d err=%v", tok3, calls.Load(), err)
	}
}
