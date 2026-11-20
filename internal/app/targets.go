package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
	"github.com/muidea/git-backup/internal/ledger"
)

// TargetCheckItem captures preflight reachability and rename information for one repository.
type TargetCheckItem struct {
	Repo      string         `json:"repo"`
	State     string         `json:"state"`
	RepoID    int64          `json:"repo_id,omitempty"`
	Canonical string         `json:"canonical,omitempty"`
	Renamed   bool           `json:"renamed"`
	LFS       config.LFSMode `json:"lfs"`
	Note      string         `json:"note"`
}

// TargetsCheckReport captures full preflight output for CLI and API consumers.
type TargetsCheckReport struct {
	Items               []TargetCheckItem `json:"items"`
	WillBecomeUnmanaged []string          `json:"will_become_unmanaged,omitempty"`
	HasIssues           bool              `json:"has_issues"`
}

// CheckTargets evaluates a given TargetConfig slice against GitHub API and local ledger.
func CheckTargets(
	ctx context.Context,
	cfg *config.Config,
	targetConfigs []config.TargetConfig,
	hooks *Hooks,
) (*TargetsCheckReport, int, error) {
	redactor := auth.NewRedactor()
	tp, err := auth.NewTokenProvider(cfg.Auth, cfg.Network.ProxyURL, redactor)
	if err != nil {
		return nil, ExitFatal, fmt.Errorf("auth init failed: %w", err)
	}
	client := github.NewClient(tp, cfg.Network.ProxyURL, redactor)
	if hooks != nil && hooks.GitHubClientCustomizer != nil {
		hooks.GitHubClientCustomizer(client, tp)
	}

	repLedger, err := ledger.LoadLedger(cfg.Storage.RootDir)
	if err != nil {
		return nil, ExitFatal, fmt.Errorf("load ledger failed: %w", err)
	}

	targets := cfg.ResolveTargetList(targetConfigs)
	metas, err := client.FetchMetadata(ctx, targets, repLedger, cfg.Sync.Concurrency, cfg.Sync.MaxRetries)
	if err != nil {
		return nil, ExitFatal, fmt.Errorf("metadata preflight failed: %s", redactor.Redact(err.Error()))
	}

	activeIDs := make(map[string]struct{}, len(metas))
	activeNames := make(map[string]struct{}, len(metas))
	report := &TargetsCheckReport{
		Items: make([]TargetCheckItem, 0, len(metas)),
	}

	for _, m := range metas {
		item := TargetCheckItem{
			Repo:      m.Target.Repo,
			State:     "ok",
			RepoID:    m.ID,
			Canonical: m.FullName,
			Renamed:   m.Renamed,
			LFS:       m.Target.EnableLFS,
			Note:      "ok",
		}
		switch m.State {
		case github.MetaMissing:
			item.State = "missing"
			item.RepoID = 0
			item.Note = "404 Not Found on GitHub (local backup if any will be retained)"
			report.HasIssues = true
		case github.MetaError:
			item.State = "error"
			item.RepoID = 0
			item.Note = redactor.Redact(fmt.Sprint(m.Err))
			report.HasIssues = true
		case github.MetaNotModified:
			item.State = "ok(304)"
		}
		if m.Renamed {
			item.Note = fmt.Sprintf("renamed on GitHub -> %s (update whitelist)", m.FullName)
		}
		if m.ID > 0 {
			activeIDs[ledger.RepoIDKey(m.ID)] = struct{}{}
		}
		if m.FullName != "" {
			activeNames[strings.ToLower(m.FullName)] = struct{}{}
		}
		activeNames[m.Target.CanonicalKey] = struct{}{}
		report.Items = append(report.Items, item)
	}

	for key, rec := range repLedger.Repositories {
		if strings.HasPrefix(key, "missing:") {
			continue
		}
		if _, ok := activeIDs[key]; ok {
			continue
		}
		if _, ok := activeNames[strings.ToLower(rec.FullName)]; ok {
			continue
		}
		if rec.State != ledger.StatusUnmanaged {
			report.WillBecomeUnmanaged = append(report.WillBecomeUnmanaged, rec.FullName)
		}
	}

	code := ExitOK
	if report.HasIssues {
		code = ExitPartial
	}
	return report, code, nil
}

// RunTargetsCheck validates whitelist config, checks GitHub reachability/renames, and lists unmanaged local repos.
func RunTargetsCheck(ctx context.Context, cfg *config.Config, out io.Writer, hooks *Hooks) int {
	if out == nil {
		out = os.Stdout
	}
	tf, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets)
	if err != nil {
		fmt.Fprintf(out, "ERROR: load targets.json failed: %v\n", err)
		return ExitFatal
	}

	report, code, err := CheckTargets(ctx, cfg, tf.Targets, hooks)
	if err != nil {
		fmt.Fprintf(out, "ERROR: %v\n", err)
		return code
	}

	fmt.Fprintf(out, "=== Targets Preflight Check (%d whitelisted) ===\n", len(report.Items))
	fmt.Fprintf(out, "%-30s %-12s %-10s %-8s %s\n", "TARGET", "STATE", "REPO_ID", "LFS", "NOTE")
	for _, item := range report.Items {
		idStr := "-"
		if item.RepoID > 0 {
			idStr = fmt.Sprintf("%d", item.RepoID)
		}
		fmt.Fprintf(out, "%-30s %-12s %-10s %-8s %s\n", item.Repo, item.State, idStr, string(item.LFS), item.Note)
	}

	if len(report.WillBecomeUnmanaged) > 0 {
		fmt.Fprintf(out, "\n[!] Repositories that will become UNMANAGED (policy=%s):\n", cfg.Defaults.UnmanagedPolicy)
		for _, name := range report.WillBecomeUnmanaged {
			fmt.Fprintf(out, "  - %s\n", name)
		}
	}

	return code
}

// RunTargetsList prints the active whitelist from targets.json (migrating from config.yaml if needed).
func RunTargetsList(cfg *config.Config, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	tf, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load targets: %v\n", err)
		return ExitFatal
	}
	resolved := cfg.ResolveTargetList(tf.Targets)
	fmt.Fprintf(out, "=== Active Whitelist (%d repositories) ===\n", len(resolved))
	fmt.Fprintf(out, "%-32s %-8s %-10s %-14s %-12s\n", "REPO", "LFS", "PR_REFS", "SKIP_UNCHANGED", "UPDATE_TIMEOUT")
	for _, t := range resolved {
		fmt.Fprintf(out, "%-32s %-8s %-10t %-14t %-12s\n", t.Repo, string(t.EnableLFS), t.MirrorPRRefs, t.SkipUnchanged, t.UpdateTimeout)
	}
	return ExitOK
}

// RunTargetsAdd adds a repository to targets.json under .backup.lock.
func RunTargetsAdd(ctx context.Context, cfg *config.Config, tc config.TargetConfig, checkRemote bool, out io.Writer, hooks *Hooks) int {
	if out == nil {
		out = os.Stdout
	}
	norm, err := config.NormalizeRepoName(tc.Repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}
	tc.Repo = norm

	if checkRemote {
		report, code, cErr := CheckTargets(ctx, cfg, []config.TargetConfig{tc}, hooks)
		if cErr != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", cErr)
			return ExitFatal
		}
		if code != ExitOK || len(report.Items) == 0 || report.Items[0].State == "missing" || report.Items[0].State == "error" {
			note := "unknown error"
			if len(report.Items) > 0 {
				note = report.Items[0].Note
			}
			fmt.Fprintf(os.Stderr, "ERROR: preflight check failed for %s: %s\n", tc.Repo, note)
			return code
		}
		if report.Items[0].Canonical != "" {
			tc.Repo = report.Items[0].Canonical
		}
	}

	lock, err := ledger.AcquireLock(cfg.Storage.RootDir)
	if err != nil {
		if errors.Is(err, ledger.ErrLockBusy) {
			fmt.Fprintln(os.Stderr, "ERROR: backup lock is held by another process; retry shortly")
			return ExitFatal
		}
		fmt.Fprintf(os.Stderr, "ERROR: acquire lock: %v\n", err)
		return ExitFatal
	}
	defer func() { _ = lock.Release() }()

	tf, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load targets: %v\n", err)
		return ExitFatal
	}
	if err := tf.Add(tc); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}
	if err := ledger.SaveTargets(cfg.Storage.RootDir, tf); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: save targets.json: %v\n", err)
		return ExitFatal
	}
	fmt.Fprintf(out, "Added %s to whitelist (%s)\n", tc.Repo, ledger.TargetsPath(cfg.Storage.RootDir))
	return ExitOK
}

// RunTargetsRemove removes a repository from targets.json and marks its ledger entry unmanaged.
func RunTargetsRemove(cfg *config.Config, repo string, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	lock, err := ledger.AcquireLock(cfg.Storage.RootDir)
	if err != nil {
		if errors.Is(err, ledger.ErrLockBusy) {
			fmt.Fprintln(os.Stderr, "ERROR: backup lock is held by another process; retry shortly")
			return ExitFatal
		}
		fmt.Fprintf(os.Stderr, "ERROR: acquire lock: %v\n", err)
		return ExitFatal
	}
	defer func() { _ = lock.Release() }()

	tf, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load targets: %v\n", err)
		return ExitFatal
	}
	name, removed, err := tf.Remove(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}
	if !removed {
		fmt.Fprintf(os.Stderr, "ERROR: repository %q not found in targets\n", name)
		return ExitFatal
	}
	if err := ledger.SaveTargets(cfg.Storage.RootDir, tf); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: save targets.json: %v\n", err)
		return ExitFatal
	}

	// Mark ledger entry unmanaged immediately so status reflects the change before next sync.
	if repLedger, lErr := ledger.LoadLedger(cfg.Storage.RootDir); lErr == nil {
		if rec := repLedger.FindByFullName(name); rec != nil {
			rec.State = ledger.StatusUnmanaged
			rec.LastError = ""
			repLedger.UpdatedAt = time.Now().UTC()
			_ = ledger.SaveLedger(cfg.Storage.RootDir, repLedger)
		}
	}

	fmt.Fprintf(out, "Removed %s from whitelist (local backup retained/archived per unmanaged_policy=%s)\n", name, cfg.Defaults.UnmanagedPolicy)
	return ExitOK
}
