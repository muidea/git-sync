package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAndResolveTargets(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
storage:
  root_dir: "/data/github-backups"
auth:
  mode: "pat"
  token_env: "MY_TOKEN"
defaults:
  enable_lfs: "auto"
  mirror_pr_refs: true
  clone_timeout: "1h"
  update_timeout: "15m"
targets:
  - repo: "muidea/clawStation"
    enable_lfs: true
    update_timeout: "30m"
  - repo: "torvalds/linux"
    clone_timeout: "4h"
    mirror_pr_refs: false
    skip_unchanged: false
sync:
  concurrency: 4
  skip_unchanged: true
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	resolved := cfg.ResolveTargets()
	if len(resolved) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(resolved))
	}

	if resolved[0].EnableLFS != LFSTrue || resolved[0].UpdateTimeout != 30*time.Minute || !resolved[0].MirrorPRRefs || !resolved[0].SkipUnchanged {
		t.Fatalf("unexpected resolved[0]: %+v", resolved[0])
	}
	if resolved[1].EnableLFS != LFSAuto || resolved[1].CloneTimeout != 4*time.Hour || resolved[1].MirrorPRRefs || resolved[1].SkipUnchanged {
		t.Fatalf("unexpected resolved[1]: %+v", resolved[1])
	}
}

func TestValidateRejectsCaseInsensitiveDuplicates(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
storage:
  root_dir: "/data/github-backups"
targets:
  - repo: "Muidea/ClawStation"
  - repo: "muidea/clawstation"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestDiscoverConfigPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	envCfg := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(envCfg, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DefaultConfigEnv, envCfg)

	got, err := DiscoverConfigPath("/explicit/config.yaml")
	if err != nil || got != "/explicit/config.yaml" {
		t.Fatalf("expected explicit flag, got %q, err=%v", got, err)
	}

	got, err = DiscoverConfigPath("")
	if err != nil || got != envCfg {
		t.Fatalf("expected env config %q, got %q, err=%v", envCfg, got, err)
	}
}

func TestNormalizeRepoName(t *testing.T) {
	cases := map[string]string{
		"muidea/magicCommon":                   "muidea/magicCommon",
		"git@github.com:muidea/magicCommon.git": "muidea/magicCommon",
		"https://github.com/muidea/magicRunner": "muidea/magicRunner",
		"https://github.com/muidea/git-sync.git/": "muidea/git-sync",
	}
	for in, want := range cases {
		got, err := NormalizeRepoName(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeRepoName(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeRepoName("invalid-repo"); err == nil {
		t.Fatal("expected error for invalid repo format")
	}
}
