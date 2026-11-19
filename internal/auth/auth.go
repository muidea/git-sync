package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

const (
	DefaultGitHubAPIURL = "https://api.github.com"
	tokenRefreshMargin  = 10 * time.Minute
)

// Redactor masks sensitive credentials in logs and persisted error messages.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

func NewRedactor() *Redactor {
	return &Redactor{}
}

// Add registers a literal secret string (and its basic-auth encoded form if needed).
func (r *Redactor) Add(secret string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.secrets {
		if s == secret {
			return
		}
	}
	r.secrets = append(r.secrets, secret)
}

// AddURL registers credentials embedded in a URL (e.g. proxy user:pass or webhook token query/path).
func (r *Redactor) AddURL(rawURL string, redactFull bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return
	}
	if redactFull {
		r.Add(rawURL)
	}
	if u, err := url.Parse(rawURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			r.Add(pw)
		}
		r.Add(u.User.String())
	}
}

// Redact replaces all registered secrets in input with [REDACTED].
func (r *Redactor) Redact(input string) string {
	if r == nil || input == "" {
		return input
	}
	r.mu.RLock()
	secrets := make([]string, len(r.secrets))
	copy(secrets, r.secrets)
	r.mu.RUnlock()

	out := input
	for _, s := range secrets {
		if s != "" {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	return out
}

// TokenProvider supplies valid GitHub tokens and refreshes GitHub App tokens before expiry.
type TokenProvider struct {
	mode           string
	patToken       string
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	apiBaseURL     string
	httpClient     *http.Client
	redactor       *Redactor
	nowFunc        func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTokenProvider constructs a TokenProvider from config and registers secrets with redactor.
func NewTokenProvider(cfg config.AuthConfig, proxyURL string, redactor *Redactor) (*TokenProvider, error) {
	if redactor == nil {
		redactor = NewRedactor()
	}
	redactor.AddURL(proxyURL, false)

	tp := &TokenProvider{
		mode:       cfg.Mode,
		apiBaseURL: DefaultGitHubAPIURL,
		httpClient: HTTPClient(proxyURL, 30*time.Second),
		redactor:   redactor,
		nowFunc:    time.Now,
	}

	switch cfg.Mode {
	case "pat":
		tok := strings.TrimSpace(os.Getenv(cfg.TokenEnv))
		if tok == "" {
			return nil, fmt.Errorf("environment variable %s is empty or unset", cfg.TokenEnv)
		}
		tp.patToken = tok
		tp.registerTokenSecret(tok)
	case "github_app":
		keyBytes, err := os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read github app private key %q: %w", cfg.PrivateKeyPath, err)
		}
		pk, err := parseRSAPrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse github app private key %q: %w", cfg.PrivateKeyPath, err)
		}
		tp.appID = cfg.AppID
		tp.installationID = cfg.InstallationID
		tp.privateKey = pk
	case "none":
		// Anonymous mode for public repositories only.
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.Mode)
	}
	return tp, nil
}

// SetAPIBaseURL overrides the GitHub API endpoint (used in tests).
func (p *TokenProvider) SetAPIBaseURL(baseURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apiBaseURL = strings.TrimRight(baseURL, "/")
}

// Token returns an active authentication token, refreshing GitHub App installation tokens when < 10m remain.
func (p *TokenProvider) Token(ctx context.Context) (string, error) {
	if p.mode == "none" {
		return "", nil
	}
	if p.mode == "pat" {
		return p.patToken, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.nowFunc()
	if p.token != "" && p.expiresAt.Sub(now) > tokenRefreshMargin {
		return p.token, nil
	}

	jwtStr, err := signAppJWT(p.appID, p.privateKey, now)
	if err != nil {
		return "", fmt.Errorf("sign github app JWT: %w", err)
	}
	p.redactor.Add(jwtStr)

	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", p.apiBaseURL, p.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request installation token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read installation token response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github app token exchange failed with status %d: %s", resp.StatusCode, p.redactor.Redact(string(body)))
	}

	var parsed struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode installation token response: %w", err)
	}
	if parsed.Token == "" {
		return "", errors.New("github app token exchange returned empty token")
	}
	if parsed.ExpiresAt.IsZero() {
		parsed.ExpiresAt = now.Add(time.Hour)
	}

	p.token = parsed.Token
	p.expiresAt = parsed.ExpiresAt
	p.registerTokenSecret(parsed.Token)
	return p.token, nil
}

func (p *TokenProvider) registerTokenSecret(tok string) {
	p.redactor.Add(tok)
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	p.redactor.Add(basic)
}

// GitEnv builds the subprocess environment with non-interactive flags, GIT_CONFIG_COUNT auth/HTTP stability settings, and optional proxy.
func GitEnv(baseEnv []string, token string, netCfg config.NetworkConfig) []string {
	filtered := make([]string, 0, len(baseEnv)+16)
	skipPrefixes := []string{
		"GIT_CONFIG_COUNT=",
		"GIT_CONFIG_KEY_",
		"GIT_CONFIG_VALUE_",
		"GIT_TERMINAL_PROMPT=",
		"GCM_INTERACTIVE=",
	}
	proxyURL := strings.TrimSpace(netCfg.ProxyURL)
	if proxyURL != "" {
		skipPrefixes = append(skipPrefixes,
			"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
			"http_proxy=", "https_proxy=", "all_proxy=",
		)
	}

	for _, kv := range baseEnv {
		skip := false
		for _, p := range skipPrefixes {
			if strings.HasPrefix(kv, p) {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, kv)
		}
	}

	filtered = append(filtered,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)

	type gitConfigKV struct {
		k, v string
	}
	var gitConfigs []gitConfigKV
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		gitConfigs = append(gitConfigs, gitConfigKV{
			k: "http.https://github.com/.extraheader",
			v: "AUTHORIZATION: basic " + basic,
		})
	}
	if strings.TrimSpace(netCfg.HTTPVersion) != "" {
		gitConfigs = append(gitConfigs, gitConfigKV{
			k: "http.version",
			v: netCfg.HTTPVersion,
		})
	}
	if netCfg.LowSpeedLimit > 0 && netCfg.LowSpeedTime > 0 {
		gitConfigs = append(gitConfigs,
			gitConfigKV{k: "http.lowSpeedLimit", v: strconv.Itoa(netCfg.LowSpeedLimit)},
			gitConfigKV{k: "http.lowSpeedTime", v: strconv.Itoa(netCfg.LowSpeedTime)},
		)
	}
	if len(gitConfigs) > 0 {
		filtered = append(filtered, "GIT_CONFIG_COUNT="+strconv.Itoa(len(gitConfigs)))
		for i, kv := range gitConfigs {
			filtered = append(filtered,
				fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv.k),
				fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv.v),
			)
		}
	}

	if proxyURL != "" {
		filtered = append(filtered,
			"HTTP_PROXY="+proxyURL,
			"HTTPS_PROXY="+proxyURL,
			"ALL_PROXY="+proxyURL,
			"http_proxy="+proxyURL,
			"https_proxy="+proxyURL,
			"all_proxy="+proxyURL,
		)
	}
	return filtered
}

// HTTPClient constructs an *http.Client configured with an optional proxy URL and request timeout.
func HTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := anyKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}

func signAppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	headerJSON := `{"alg":"RS256","typ":"JWT"}`
	payload, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{
		IAT: now.Add(-60 * time.Second).Unix(),
		EXP: now.Add(9 * time.Minute).Unix(),
		ISS: strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString([]byte(headerJSON)) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
