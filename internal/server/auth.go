package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

const (
	sessionCookieName = "git_backup_session"
	sessionTTL        = 24 * time.Hour
)

// AuthGuard enforces Bearer/Cookie auth and the loopback-only no-auth security rule.
type AuthGuard struct {
	token        string
	authRequired bool

	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewAuthGuard(listenAddr, apiToken string) (*AuthGuard, error) {
	tok := strings.TrimSpace(apiToken)
	if tok == "" && !config.IsLoopbackListen(listenAddr) {
		return nil, fmt.Errorf("refusing to bind server to non-loopback address %q without API token", listenAddr)
	}
	return &AuthGuard{
		token:        tok,
		authRequired: tok != "",
		sessions:     make(map[string]time.Time),
	}, nil
}

func (g *AuthGuard) AuthRequired() bool {
	return g.authRequired
}

func (g *AuthGuard) IsAuthenticated(r *http.Request) bool {
	if !g.authRequired {
		return isLoopbackRequest(r)
	}
	if authHdr := strings.TrimSpace(r.Header.Get("Authorization")); authHdr != "" {
		const prefix = "Bearer "
		if len(authHdr) > len(prefix) && strings.EqualFold(authHdr[:len(prefix)], prefix) {
			candidate := strings.TrimSpace(authHdr[len(prefix):])
			if subtle.ConstantTimeCompare([]byte(candidate), []byte(g.token)) == 1 {
				return true
			}
		}
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.sessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(g.sessions, cookie.Value)
		return false
	}
	return true
}

func (g *AuthGuard) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.authRequired {
			if !isLoopbackRequest(r) {
				writeJSONError(w, http.StatusForbidden, "loopback-only no-auth mode rejected non-loopback request")
				return
			}
			next(w, r)
			return
		}
		if !g.IsAuthenticated(r) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized: valid Bearer token or session required")
			return
		}
		next(w, r)
	}
}

func (g *AuthGuard) HandleSessionGet(w http.ResponseWriter, r *http.Request) {
	if !g.authRequired && !isLoopbackRequest(r) {
		writeJSONError(w, http.StatusForbidden, "loopback-only no-auth mode rejected non-loopback request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": g.authRequired,
		"authenticated": g.IsAuthenticated(r),
	})
}

func (g *AuthGuard) HandleSessionPost(w http.ResponseWriter, r *http.Request) {
	if !g.authRequired {
		if !isLoopbackRequest(r) {
			writeJSONError(w, http.StatusForbidden, "loopback-only no-auth mode rejected non-loopback request")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"auth_required": false, "authenticated": true})
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(g.token)) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "invalid API token")
		return
	}

	sid, err := newRandomID(24)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "generate session id failed")
		return
	}
	now := time.Now()
	g.mu.Lock()
	for k, exp := range g.sessions {
		if now.After(exp) {
			delete(g.sessions, k)
		}
	}
	g.sessions[sid] = now.Add(sessionTTL)
	g.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"auth_required": true, "authenticated": true})
}

func (g *AuthGuard) HandleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		g.mu.Lock()
		delete(g.sessions, cookie.Value)
		g.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func isLoopbackRequest(r *http.Request) bool {
	if r.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		host = strings.Trim(host, "[]")
		if !isLoopbackHost(host) {
			return false
		}
	}
	if r.Host != "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if !isLoopbackHost(strings.Trim(host, "[]")) {
			return false
		}
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !isLoopbackHost(strings.Trim(u.Hostname(), "[]")) {
			return false
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newRandomID(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
