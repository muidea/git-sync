package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
	"github.com/muidea/git-backup/internal/ledger"
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

func createBareRemoteWithCommit(t *testing.T, root, name string) (remoteBare, workDir string) {
	t.Helper()
	remoteBare = filepath.Join(root, name+".git")
	workDir = filepath.Join(root, name+"-work")
	runGit(t, "", "init", "--bare", remoteBare)
	runGit(t, "", "clone", remoteBare, workDir)
	runGit(t, workDir, "config", "user.email", "test@example.com")
	runGit(t, workDir, "config", "user.name", "Tester")
	_ = os.WriteFile(filepath.Join(workDir, "README.md"), []byte("init\n"), 0o644)
	runGit(t, workDir, "add", "README.md")
	runGit(t, workDir, "commit", "-m", "initial")
	runGit(t, workDir, "branch", "-M", "main")
	runGit(t, workDir, "push", "-u", "origin", "main")
	return remoteBare, workDir
}

func TestEndToEndSyncVerifyMissingRetainRenameAdoptAndReport(t *testing.T) {
	ctx := context.Background()
	temp := t.TempDir()
	remotesRoot := filepath.Join(temp, "remotes")
	backupRoot := filepath.Join(temp, "backups")
	promPath := filepath.Join(temp, "prom", "git_backup.prom")

	remoteA, workA := createBareRemoteWithCommit(t, remotesRoot, "muidea/alpha")
	remoteB, _ := createBareRemoteWithCommit(t, remotesRoot, "muidea/beta")
	remoteAdopt, _ := createBareRemoteWithCommit(t, remotesRoot, "muidea/adopt")

	// Pre-populate an existing bare repo for cold-start adoption test.
	adoptLocal := filepath.Join(backupRoot, "repos", "muidea", "adopt.git")
	if err := os.MkdirAll(filepath.Dir(adoptLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "", "clone", "--mirror", remoteAdopt, adoptLocal)
	runGit(t, adoptLocal, "remote", "set-url", "origin", "https://github.com/muidea/adopt.git")

	t.Setenv("E2E_GH_TOKEN", "pat_e2e_secret")

	var webhookEvents []map[string]any
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		webhookEvents = append(webhookEvents, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer whSrv.Close()
	t.Setenv("E2E_WH_URL", whSrv.URL)

	pushedA := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pushedB := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pushedAdopt := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var betaMissing atomic.Bool
	var alphaRenamed atomic.Bool

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/muidea/alpha", "/repos/muidea/AlphaV2":
			fullName := "muidea/alpha"
			if alphaRenamed.Load() {
				fullName = "muidea/AlphaV2"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        101,
				"full_name": fullName,
				"pushed_at": pushedA,
				"size":      10,
			})
		case "/repos/muidea/beta":
			if betaMissing.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        202,
				"full_name": "muidea/beta",
				"pushed_at": pushedB,
				"size":      10,
			})
		case "/repos/muidea/adopt":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        303,
				"full_name": "muidea/adopt",
				"pushed_at": pushedAdopt,
				"size":      10,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ghSrv.Close()

	cfgPath := filepath.Join(temp, "config.yaml")
	cfgYAML := `
storage:
  root_dir: "` + backupRoot + `"
  trash_retention_days: 30
  gc_interval_syncs: 25
  min_free_disk_percent: 0
  min_free_disk_gb: 0
auth:
  mode: "pat"
  token_env: "E2E_GH_TOKEN"
defaults:
  enable_lfs: "auto"
  mirror_pr_refs: true
  clone_timeout: "10m"
  update_timeout: "5m"
  unmanaged_policy: "retain"
targets:
  - repo: "muidea/alpha"
  - repo: "muidea/beta"
  - repo: "muidea/adopt"
sync:
  concurrency: 2
  skip_unchanged: true
  force_full_sync_days: 7
  max_retries: 1
safety:
  max_missing_alert_ratio: 0.5
observability:
  log_format: "text"
  log_level: "info"
  prometheus_textfile_path: "` + promPath + `"
  alert:
    webhook_type: "generic"
    webhook_url_env: "E2E_WH_URL"
    failure_threshold: 1
    alert_cooldown: "24h"
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	hooks := &Hooks{
		GitHubClientCustomizer: func(c *github.Client, tp *auth.TokenProvider) {
			c.SetEndpoints(ghSrv.URL, ghSrv.URL+"/graphql")
		},
		CloneURLOverride: func(meta github.RepoMeta) string {
			switch meta.ID {
			case 101:
				return remoteA
			case 202:
				return remoteB
			case 303:
				return remoteAdopt
			}
			switch strings.ToLower(meta.Target.Repo) {
			case "muidea/alpha", "muidea/alphav2":
				return remoteA
			case "muidea/beta":
				return remoteB
			case "muidea/adopt":
				return remoteAdopt
			}
			return ""
		},
		SkipDiskSpaceCheck: true,
		NowFunc:            func() time.Time { return now },
	}

	var logBuf bytes.Buffer
	logger := NewLogger(cfg.Observability, &logBuf)

	// Run 1: initial sync (alpha+beta cloned, adopt adopted & updated)
	if code := RunSync(ctx, cfg, logger, hooks); code != ExitOK {
		t.Fatalf("Run 1 expected ExitOK, got %d\nlogs:\n%s", code, logBuf.String())
	}
	sum1, err := ledger.LoadSummary(backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if sum1.Counts.Cloned != 2 || sum1.Counts.Updated != 1 || sum1.Counts.ManagedTotal != 3 {
		t.Fatalf("unexpected Run 1 counts: %+v", sum1.Counts)
	}

	// Verify command should pass
	var verifyOut bytes.Buffer
	if code := RunVerify(ctx, cfg, VerifyOptions{}, &verifyOut, hooks); code != ExitOK {
		t.Fatalf("Verify expected ExitOK, got %d:\n%s", code, verifyOut.String())
	}

	// Run 2: unchanged pushed_at -> all 3 up_to_date
	now = now.Add(time.Hour)
	if code := RunSync(ctx, cfg, logger, hooks); code != ExitOK {
		t.Fatalf("Run 2 expected ExitOK, got %d", code)
	}
	sum2, _ := ledger.LoadSummary(backupRoot)
	if sum2.Counts.UpToDate != 3 {
		t.Fatalf("expected 3 up_to_date on Run 2, got %+v", sum2.Counts)
	}

	// Run 3: beta returns 404 (missing) -> must retain local backup intact, exit 2, send alert; alpha gets new commit + rename
	betaMissing.Store(true)
	alphaRenamed.Store(true)
	_ = os.WriteFile(filepath.Join(workA, "new.txt"), []byte("v2\n"), 0o644)
	runGit(t, workA, "add", "new.txt")
	runGit(t, workA, "commit", "-m", "v2")
	runGit(t, workA, "push", "origin", "main")
	pushedA = pushedA.Add(2 * time.Hour)

	now = now.Add(time.Hour)
	if code := RunSync(ctx, cfg, logger, hooks); code != ExitPartial {
		t.Fatalf("Run 3 expected ExitPartial (2), got %d", code)
	}
	sum3, _ := ledger.LoadSummary(backupRoot)
	if sum3.Counts.Missing != 1 || sum3.Counts.Updated != 1 || sum3.Counts.UpToDate != 1 {
		t.Fatalf("unexpected Run 3 counts: %+v", sum3.Counts)
	}
	// Verify beta local repo is STILL retained in repos/muidea/beta.git
	if _, err := os.Stat(filepath.Join(backupRoot, "repos", "muidea", "beta.git", "HEAD")); err != nil {
		t.Fatalf("expected missing repo beta.git to be retained locally, got err=%v", err)
	}
	// Verify alpha migrated to repos/muidea/AlphaV2.git
	if _, err := os.Stat(filepath.Join(backupRoot, "repos", "muidea", "AlphaV2.git", "HEAD")); err != nil {
		t.Fatalf("expected renamed repo AlphaV2.git to exist, got err=%v", err)
	}
	if len(webhookEvents) == 0 || webhookEvents[len(webhookEvents)-1]["event_type"] != "repo_alert" {
		t.Fatalf("expected repo_alert webhook event, got %+v", webhookEvents)
	}

	// Run 4: beta comes back on GitHub -> resumes sync & fires recovery webhook
	betaMissing.Store(false)
	now = now.Add(time.Hour)
	if code := RunSync(ctx, cfg, logger, hooks); code != ExitOK {
		t.Fatalf("Run 4 expected ExitOK after beta restored, got %d", code)
	}
	if webhookEvents[len(webhookEvents)-1]["event_type"] != "recovery" {
		t.Fatalf("expected recovery webhook event, got %+v", webhookEvents[len(webhookEvents)-1])
	}

	// Test single-repo sync does not mark other targets unmanaged
	singleRes := RunSyncWithOptions(ctx, cfg, logger, SyncOptions{RepoFilter: []string{"muidea/alpha"}}, hooks)
	if singleRes.ExitCode != ExitOK || singleRes.Summary.Counts.ManagedTotal != 3 || singleRes.Summary.Counts.Unmanaged != 0 {
		t.Fatalf("unexpected single-repo sync result: %+v", singleRes)
	}

	// Test targets --check with adopt removed from targets.json
	tf, _, err := ledger.LoadOrMigrateTargets(backupRoot, cfg.Targets)
	if err != nil {
		t.Fatal(err)
	}
	if _, removed, rErr := tf.Remove("muidea/adopt"); rErr != nil || !removed {
		t.Fatalf("failed removing adopt from targets.json: removed=%v err=%v", removed, rErr)
	}
	if err := ledger.SaveTargets(backupRoot, tf); err != nil {
		t.Fatal(err)
	}
	var targetsOut bytes.Buffer
	if code := RunTargetsCheck(ctx, cfg, &targetsOut, hooks); code != ExitOK {
		t.Fatalf("RunTargetsCheck expected ExitOK, got %d\n%s", code, targetsOut.String())
	}
	if !strings.Contains(targetsOut.String(), "muidea/adopt") || !strings.Contains(targetsOut.String(), "UNMANAGED") {
		t.Fatalf("expected targets check to report unmanaged muidea/adopt, got:\n%s", targetsOut.String())
	}

	// Test status & weekly report
	var statusOut bytes.Buffer
	if code := RunStatus(StatusOptions{RootDir: backupRoot}, &statusOut); code != ExitOK {
		t.Fatalf("RunStatus failed: %d", code)
	}
	if !strings.Contains(statusOut.String(), "muidea/AlphaV2") {
		t.Fatalf("expected status output to include muidea/AlphaV2:\n%s", statusOut.String())
	}

	var reportOut bytes.Buffer
	if code := RunReport(ctx, cfg, &reportOut, hooks); code != ExitOK {
		t.Fatalf("RunReport failed: %d", code)
	}
	if webhookEvents[len(webhookEvents)-1]["event_type"] != "weekly_report" {
		t.Fatalf("expected weekly_report webhook event, got %+v", webhookEvents[len(webhookEvents)-1])
	}
}
