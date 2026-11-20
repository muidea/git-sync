package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/app"
	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func makeBareRemote(t *testing.T, root, name string) string {
	t.Helper()
	bare := filepath.Join(root, name+".git")
	work := filepath.Join(root, name+"-work")
	runGit(t, "", "init", "--bare", bare)
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Tester")
	_ = os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "init")
	runGit(t, work, "branch", "-M", "main")
	runGit(t, work, "push", "-u", "origin", "main")
	return bare
}

func TestLoopbackOnlyNoAuthRule(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageConfig{RootDir: tmp},
		Auth:    config.AuthConfig{Mode: "none"},
		Server: config.ServerConfig{
			Listen:      "0.0.0.0:8080",
			APITokenEnv: "TEST_SERVER_API_TOKEN_UNSET",
		},
	}

	if _, err := New(cfg, app.NewLogger(config.ObservabilityConfig{LogFormat: "text", LogLevel: "info"}, io.Discard), nil); err == nil {
		t.Fatal("expected New() to refuse binding 0.0.0.0 without API token")
	}

	cfg.Server.Listen = "127.0.0.1:8080"
	srv, err := New(cfg, app.NewLogger(config.ObservabilityConfig{LogFormat: "text", LogLevel: "info"}, io.Discard), nil)
	if err != nil {
		t.Fatalf("expected loopback bind without token to succeed, got %v", err)
	}

	// Reject non-loopback Origin header on loopback no-auth mode (DNS rebinding protection).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Origin, got %d", rec.Code)
	}
}

func TestAPIAuthTargetsLifecycleSyncVerifyAndUI(t *testing.T) {
	tmp := t.TempDir()
	remotesRoot := filepath.Join(tmp, "remotes")
	backupRoot := filepath.Join(tmp, "backups")
	remoteAlpha := makeBareRemote(t, remotesRoot, "muidea/alpha")

	t.Setenv("TEST_API_TOK", "api_secret_xyz")
	t.Setenv("TEST_GH_TOK", "gh_secret_123")

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/muidea/alpha" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        111,
				"full_name": "muidea/alpha",
				"pushed_at": "2026-11-20T05:00:00Z",
				"size":      10,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ghSrv.Close()

	cfgPath := filepath.Join(tmp, "config.yaml")
	cfgYAML := `
storage:
  root_dir: "` + backupRoot + `"
  min_free_disk_percent: 0
  min_free_disk_gb: 0
auth:
  mode: "pat"
  token_env: "TEST_GH_TOK"
sync:
  concurrency: 2
server:
  listen: "0.0.0.0:8080"
  api_token_env: "TEST_API_TOK"
  sync_interval: "0s"
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	hooks := &app.Hooks{
		GitHubClientCustomizer: func(c *github.Client, _ *auth.TokenProvider) {
			c.SetEndpoints(ghSrv.URL, ghSrv.URL+"/graphql")
		},
		CloneURLOverride: func(meta github.RepoMeta) string {
			return remoteAlpha
		},
		SkipDiskSpaceCheck: true,
	}

	srv, err := New(cfg, app.NewLogger(cfg.Observability, io.Discard), hooks)
	if err != nil {
		t.Fatalf("New server failed: %v", err)
	}
	handler := srv.Handler()

	// 1. Unauthenticated API call returns 401; /healthz and / return 200.
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	unauthRec := httptest.NewRecorder()
	handler.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", unauthRec.Code)
	}

	uiReq := httptest.NewRequest(http.MethodGet, "/", nil)
	uiRec := httptest.NewRecorder()
	handler.ServeHTTP(uiRec, uiReq)
	if uiRec.Code != http.StatusOK || !strings.Contains(uiRec.Body.String(), "git-backup") {
		t.Fatalf("expected embedded Web UI HTML, got code=%d", uiRec.Code)
	}

	// 2. Cookie session login works
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"token":"api_secret_xyz"}`))
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on session login, got %d", loginRec.Code)
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly session cookie, got %+v", cookies)
	}

	doAuth := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer api_secret_xyz")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 3. Add target with ?check=true
	addRec := doAuth(http.MethodPost, "/api/v1/targets?check=true", `{"repo":"git@github.com:muidea/alpha.git"}`)
	if addRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 on add target, got %d: %s", addRec.Code, addRec.Body.String())
	}

	// 4. Patch target
	patchRec := doAuth(http.MethodPatch, "/api/v1/targets/muidea/alpha", `{"enable_lfs":"false","update_timeout":"25m"}`)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on patch target, got %d: %s", patchRec.Code, patchRec.Body.String())
	}

	// 5. Trigger async sync & poll job until completion
	syncRec := doAuth(http.MethodPost, "/api/v1/sync", `{"repos":["muidea/alpha"]}`)
	if syncRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 on sync, got %d: %s", syncRec.Code, syncRec.Body.String())
	}
	var syncResp struct {
		Job JobSnapshot `json:"job"`
	}
	_ = json.Unmarshal(syncRec.Body.Bytes(), &syncResp)

	waitJob := func(id string) JobSnapshot {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			jr := doAuth(http.MethodGet, "/api/v1/jobs/"+id, "")
			var j JobSnapshot
			_ = json.Unmarshal(jr.Body.Bytes(), &j)
			if j.Status == JobSucceeded || j.Status == JobPartialFailure || j.Status == JobFailed {
				return j
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for job %s", id)
		return JobSnapshot{}
	}

	doneSync := waitJob(syncResp.Job.ID)
	if doneSync.Status != JobSucceeded || doneSync.Summary == nil || doneSync.Summary.Counts.Cloned != 1 {
		t.Fatalf("unexpected sync job outcome: %+v", doneSync)
	}

	// 6. Trigger async verify --fsck
	verRec := doAuth(http.MethodPost, "/api/v1/verify", `{"repos":["muidea/alpha"],"fsck":true}`)
	if verRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 on verify, got %d: %s", verRec.Code, verRec.Body.String())
	}
	var verResp struct {
		Job JobSnapshot `json:"job"`
	}
	_ = json.Unmarshal(verRec.Body.Bytes(), &verResp)
	doneVer := waitJob(verResp.Job.ID)
	if doneVer.Status != JobSucceeded || len(doneVer.VerifyResults) != 1 || !doneVer.VerifyResults[0].OK {
		t.Fatalf("unexpected verify job outcome: %+v", doneVer)
	}

	// 7. Metrics & Repo Detail
	metRec := doAuth(http.MethodGet, "/metrics", "")
	if metRec.Code != http.StatusOK || !strings.Contains(metRec.Body.String(), "git_backup_repos_total") {
		t.Fatalf("unexpected /metrics output: %d %s", metRec.Code, metRec.Body.String())
	}
	detRec := doAuth(http.MethodGet, "/api/v1/repos/muidea/alpha", "")
	if detRec.Code != http.StatusOK || !strings.Contains(detRec.Body.String(), "refs/heads/main") {
		t.Fatalf("unexpected repo detail: %d %s", detRec.Code, detRec.Body.String())
	}

	// 8. Delete target (retains local directory as unmanaged)
	delRec := doAuth(http.MethodDelete, "/api/v1/targets/muidea/alpha", "")
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete target, got %d: %s", delRec.Code, delRec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(backupRoot, "repos", "muidea", "alpha.git", "HEAD")); err != nil {
		t.Fatalf("expected local mirror directory to be retained on DELETE target, err=%v", err)
	}
}
