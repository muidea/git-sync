package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
)

func TestRunInitAndServiceInstallUninstall(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "etc", "config.yaml")
	secPath := filepath.Join(tmp, "etc", "secret.env")
	rootDir := filepath.Join(tmp, "backups")
	unitDir := filepath.Join(tmp, "systemd")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/muidea/magicCommon":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        129411470,
				"full_name": "muidea/magicCommon",
				"size":      100,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	hooks := &Hooks{
		GitHubClientCustomizer: func(c *github.Client, _ *auth.TokenProvider) {
			c.SetEndpoints(srv.URL, srv.URL+"/graphql")
		},
		SkipGitVersionCheck: true,
	}

	var out bytes.Buffer
	code := RunInit(ctx, InitOptions{
		ConfigPath:     cfgPath,
		RootDir:        rootDir,
		Repos:          []string{"git@github.com:muidea/magicCommon.git"},
		AuthMode:       "pat",
		Token:          "pat_init_secret",
		TokenEnv:       "INIT_TEST_TOKEN",
		SecretFile:     secPath,
		InstallService: true,
		UserService:    true,
		ServiceUnitDir: unitDir,
		SkipSystemctl:  true,
	}, &out, hooks)
	if code != ExitOK {
		t.Fatalf("RunInit expected ExitOK, got %d\noutput:\n%s", code, out.String())
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load generated config failed: %v", err)
	}
	if cfg.Storage.RootDir != rootDir || len(cfg.Targets) != 1 || cfg.Targets[0].Repo != "muidea/magicCommon" {
		t.Fatalf("unexpected generated config: %+v", cfg)
	}

	secInfo, err := os.Stat(secPath)
	if err != nil {
		t.Fatalf("secret.env missing: %v", err)
	}
	if secInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected secret.env perm 0600, got %o", secInfo.Mode().Perm())
	}

	// Verify systemd unit files rendered and written.
	svcBytes, err := os.ReadFile(filepath.Join(unitDir, "git-backup.service"))
	if err != nil {
		t.Fatalf("git-backup.service missing: %v", err)
	}
	svcStr := string(svcBytes)
	if !strings.Contains(svcStr, "EnvironmentFile=-"+secPath) || !strings.Contains(svcStr, "--config "+cfgPath) || !strings.Contains(svcStr, "WantedBy=default.target") {
		t.Fatalf("unexpected rendered service unit:\n%s", svcStr)
	}

	// Verify dry-run with --with-report
	var dryOut bytes.Buffer
	dryCode := RunServiceInstall(ctx, ServiceOptions{
		ConfigPath: cfgPath,
		SecretFile: secPath,
		BinaryPath: "/usr/local/bin/git-backup",
		WithReport: true,
		DryRun:     true,
	}, &dryOut)
	if dryCode != ExitOK || !strings.Contains(dryOut.String(), "git-backup-report.service") || !strings.Contains(dryOut.String(), "git-backup-report.timer") {
		t.Fatalf("unexpected dry-run output (code=%d):\n%s", dryCode, dryOut.String())
	}

	// Verify uninstall cleans up files cleanly
	var unOut bytes.Buffer
	unCode := RunServiceUninstall(ctx, ServiceOptions{
		UnitDirOverride: unitDir,
		SkipSystemctl:   true,
	}, &unOut)
	if unCode != ExitOK {
		t.Fatalf("RunServiceUninstall failed: %d", unCode)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "git-backup.service")); !os.IsNotExist(err) {
		t.Fatalf("expected git-backup.service to be removed, err=%v", err)
	}
}
