package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/gitops"
	"github.com/muidea/git-backup/internal/ledger"
)

type InitOptions struct {
	ConfigPath     string
	RootDir        string
	Repos          []string
	AuthMode       string
	Token          string
	TokenEnv       string
	SecretFile     string
	WebhookType    string
	WebhookURL     string
	WebhookURLEnv  string
	Force          bool
	SkipCheck      bool
	InstallService bool
	UserService    bool
	ServiceUnitDir string
	SkipSystemctl  bool
	In             io.Reader
}

const initConfigTmpl = `storage:
  root_dir: {{printf "%q" .RootDir}}
  trash_retention_days: 30
  gc_interval_syncs: 25
  min_free_disk_percent: 10
  min_free_disk_gb: 20

auth:
  mode: {{printf "%q" .AuthMode}}
{{- if eq .AuthMode "pat"}}
  token_env: {{printf "%q" .TokenEnv}}
{{- end}}

defaults:
  enable_lfs: "auto"
  mirror_pr_refs: true
  clone_timeout: "1h"
  update_timeout: "15m"
  unmanaged_policy: "retain"

targets:
{{- range .Repos}}
  - repo: {{printf "%q" .}}
{{- end}}

sync:
  concurrency: 6
  skip_unchanged: true
  force_full_sync_days: 7
  max_retries: 3

safety:
  max_missing_alert_ratio: 0.30

observability:
  log_format: "json"
  log_level: "info"
{{- if .WebhookType}}
  alert:
    webhook_type: {{printf "%q" .WebhookType}}
    webhook_url_env: {{printf "%q" .WebhookURLEnv}}
    failure_threshold: 2
    alert_cooldown: "24h"
{{- end}}
`

// RunInit initializes config.yaml, secret.env, backup root_dir, and optionally validates & installs systemd service.
func RunInit(ctx context.Context, opts InitOptions, out io.Writer, hooks *Hooks) int {
	if out == nil {
		out = os.Stdout
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}

	if hooks == nil || !hooks.SkipGitVersionCheck {
		if err := gitops.CheckGitVersion(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			return ExitFatal
		}
		if !gitops.HasGitLFS(ctx) {
			fmt.Fprintln(out, "NOTE: git-lfs not found on host; LFS repositories will fail unless git-lfs is installed.")
		}
	}

	reader := bufio.NewReader(opts.In)
	interactive := isInteractiveStdin(opts.In)

	// 1. Resolve config path & data root defaults based on privilege.
	if strings.TrimSpace(opts.ConfigPath) == "" {
		defaultCfg := defaultInitConfigPath()
		if interactive && len(opts.Repos) == 0 {
			opts.ConfigPath = promptLine(reader, out, fmt.Sprintf("Config file path [%s]: ", defaultCfg), defaultCfg)
		} else {
			opts.ConfigPath = defaultCfg
		}
	}
	absCfg, err := filepath.Abs(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: invalid --config path %q: %v\n", opts.ConfigPath, err)
		return ExitFatal
	}
	opts.ConfigPath = absCfg

	if !opts.Force {
		if _, statErr := os.Stat(opts.ConfigPath); statErr == nil {
			fmt.Fprintf(os.Stderr, "ERROR: config file %q already exists (pass --force to overwrite)\n", opts.ConfigPath)
			return ExitFatal
		}
	}

	if strings.TrimSpace(opts.RootDir) == "" {
		defaultRoot := defaultInitRootDir()
		if interactive && len(opts.Repos) == 0 {
			opts.RootDir = promptLine(reader, out, fmt.Sprintf("Backup storage directory [%s]: ", defaultRoot), defaultRoot)
		} else {
			opts.RootDir = defaultRoot
		}
	}
	absRoot, err := filepath.Abs(opts.RootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: invalid --root path %q: %v\n", opts.RootDir, err)
		return ExitFatal
	}
	opts.RootDir = absRoot

	// 2. Collect and normalize target repositories.
	if len(opts.Repos) == 0 && interactive {
		raw := promptLine(reader, out, "Repositories to backup (comma-separated owner/repo or GitHub URLs): ", "")
		opts.Repos = splitRepoList(raw)
	}
	normalizedRepos, err := normalizeRepoList(opts.Repos)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}
	if len(normalizedRepos) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: at least one repository is required (--repos owner/repo)")
		return ExitFatal
	}

	// 3. Resolve auth mode & token.
	if strings.TrimSpace(opts.AuthMode) == "" {
		if interactive {
			opts.AuthMode = strings.ToLower(promptLine(reader, out, "Auth mode (pat|none) [pat]: ", "pat"))
		} else {
			opts.AuthMode = "pat"
		}
	}
	opts.AuthMode = strings.ToLower(strings.TrimSpace(opts.AuthMode))
	if opts.AuthMode != "pat" && opts.AuthMode != "none" {
		fmt.Fprintf(os.Stderr, "ERROR: init supports auth mode 'pat' or 'none' (got %q)\n", opts.AuthMode)
		return ExitFatal
	}
	if strings.TrimSpace(opts.TokenEnv) == "" {
		opts.TokenEnv = "GITHUB_BACKUP_TOKEN"
	}
	if opts.AuthMode == "pat" && strings.TrimSpace(opts.Token) == "" {
		if envVal := strings.TrimSpace(os.Getenv(opts.TokenEnv)); envVal != "" {
			opts.Token = envVal
		} else if interactive {
			opts.Token = promptLine(reader, out, "GitHub Fine-grained PAT (Metadata:Read + Contents:Read): ", "")
		}
	}
	if strings.TrimSpace(opts.WebhookURLEnv) == "" {
		opts.WebhookURLEnv = "BACKUP_ALERT_WEBHOOK"
	}

	if strings.TrimSpace(opts.SecretFile) == "" {
		opts.SecretFile = filepath.Join(filepath.Dir(opts.ConfigPath), "secret.env")
	}

	// 4. Render & write config.yaml, secret.env, and create root_dir.
	cfgBytes, err := renderInitConfig(opts, normalizedRepos)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: render config: %v\n", err)
		return ExitFatal
	}

	if err := os.MkdirAll(opts.RootDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: create storage root_dir %q: %v\n", opts.RootDir, err)
		return ExitFatal
	}
	if err := ledger.WriteFileAtomic(opts.ConfigPath, cfgBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: write config %q: %v\n", opts.ConfigPath, err)
		return ExitFatal
	}
	fmt.Fprintf(out, "Created config: %s\n", opts.ConfigPath)

	if opts.AuthMode == "pat" && (strings.TrimSpace(opts.Token) != "" || strings.TrimSpace(opts.WebhookURL) != "") {
		var secBuf strings.Builder
		if strings.TrimSpace(opts.Token) != "" {
			fmt.Fprintf(&secBuf, "%s=%s\n", opts.TokenEnv, strings.TrimSpace(opts.Token))
			_ = os.Setenv(opts.TokenEnv, strings.TrimSpace(opts.Token))
		}
		if strings.TrimSpace(opts.WebhookURL) != "" {
			fmt.Fprintf(&secBuf, "%s=%s\n", opts.WebhookURLEnv, strings.TrimSpace(opts.WebhookURL))
		}
		if err := ledger.WriteFileAtomic(opts.SecretFile, []byte(secBuf.String()), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: write secret file %q: %v\n", opts.SecretFile, err)
			return ExitFatal
		}
		fmt.Fprintf(out, "Created secret file (0600): %s\n", opts.SecretFile)
	}

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: validate generated config: %v\n", err)
		return ExitFatal
	}

	// 5. Preflight targets check unless skipped.
	if !opts.SkipCheck {
		if opts.AuthMode == "pat" && strings.TrimSpace(os.Getenv(opts.TokenEnv)) == "" {
			fmt.Fprintf(out, "NOTE: %s is not set yet; skipping remote check. Set it in %s and run `git-backup targets --check --config %s`.\n",
				opts.TokenEnv, opts.SecretFile, opts.ConfigPath)
		} else {
			code := RunTargetsCheck(ctx, cfg, out, hooks)
			if code != ExitOK {
				fmt.Fprintln(out, "WARNING: targets preflight reported issues; inspect output above before running sync.")
				return code
			}
		}
	}

	// 6. Optional systemd service installation.
	if opts.InstallService {
		svcCode := RunServiceInstall(ctx, ServiceOptions{
			ConfigPath:      opts.ConfigPath,
			SecretFile:      opts.SecretFile,
			UserMode:        opts.UserService || os.Geteuid() != 0,
			UnitDirOverride: opts.ServiceUnitDir,
			SkipSystemctl:   opts.SkipSystemctl,
		}, out)
		if svcCode != ExitOK {
			return svcCode
		}
	} else {
		fmt.Fprintf(out, "\nNext steps:\n  1. Run initial sync: git-backup sync --config %s\n", opts.ConfigPath)
		if os.Geteuid() == 0 {
			fmt.Fprintf(out, "  2. Enable scheduled timer: git-backup service install --config %s\n", opts.ConfigPath)
		} else {
			fmt.Fprintf(out, "  2. Enable user timer: git-backup service install --user --config %s\n", opts.ConfigPath)
		}
	}
	return ExitOK
}

func defaultInitConfigPath() string {
	if os.Geteuid() == 0 {
		return config.SystemConfigPath
	}
	if u := config.UserConfigPath(); u != "" {
		return u
	}
	return "./config.yaml"
}

func defaultInitRootDir() string {
	if os.Geteuid() == 0 {
		return "/data/github-backups"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "github-backups")
	}
	return "./github-backups"
}

func splitRepoList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normalizeRepoList(rawList []string) ([]string, error) {
	seen := make(map[string]struct{}, len(rawList))
	out := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		for _, item := range splitRepoList(raw) {
			norm, err := config.NormalizeRepoName(item)
			if err != nil {
				return nil, err
			}
			key := strings.ToLower(norm)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, norm)
		}
	}
	return out, nil
}

func renderInitConfig(opts InitOptions, repos []string) ([]byte, error) {
	t, err := template.New("config").Parse(initConfigTmpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, struct {
		RootDir       string
		AuthMode      string
		TokenEnv      string
		Repos         []string
		WebhookType   string
		WebhookURLEnv string
	}{
		RootDir:       opts.RootDir,
		AuthMode:      opts.AuthMode,
		TokenEnv:      opts.TokenEnv,
		Repos:         repos,
		WebhookType:   strings.TrimSpace(opts.WebhookType),
		WebhookURLEnv: opts.WebhookURLEnv,
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isInteractiveStdin(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func promptLine(reader *bufio.Reader, out io.Writer, label, fallback string) string {
	fmt.Fprint(out, label)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fallback
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}
