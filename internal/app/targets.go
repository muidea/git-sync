package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
	"github.com/muidea/git-backup/internal/ledger"
)

// RunTargetsCheck validates whitelist config, checks GitHub reachability/renames, and lists unmanaged local repos.
func RunTargetsCheck(ctx context.Context, cfg *config.Config, out io.Writer, hooks *Hooks) int {
	if out == nil {
		out = os.Stdout
	}
	redactor := auth.NewRedactor()
	tp, err := auth.NewTokenProvider(cfg.Auth, cfg.Network.ProxyURL, redactor)
	if err != nil {
		fmt.Fprintf(out, "ERROR: auth init failed: %v\n", err)
		return ExitFatal
	}
	client := github.NewClient(tp, cfg.Network.ProxyURL, redactor)
	if hooks != nil && hooks.GitHubClientCustomizer != nil {
		hooks.GitHubClientCustomizer(client, tp)
	}

	repLedger, err := ledger.LoadLedger(cfg.Storage.RootDir)
	if err != nil {
		fmt.Fprintf(out, "ERROR: load ledger failed: %v\n", err)
		return ExitFatal
	}

	targets := cfg.ResolveTargets()
	metas, err := client.FetchMetadata(ctx, targets, repLedger, cfg.Sync.Concurrency, cfg.Sync.MaxRetries)
	if err != nil {
		fmt.Fprintf(out, "ERROR: metadata preflight failed: %s\n", redactor.Redact(err.Error()))
		return ExitFatal
	}

	activeIDs := make(map[string]struct{}, len(metas))
	activeNames := make(map[string]struct{}, len(metas))
	hasIssues := false

	fmt.Fprintf(out, "=== Targets Preflight Check (%d whitelisted) ===\n", len(targets))
	fmt.Fprintf(out, "%-30s %-12s %-10s %-8s %s\n", "TARGET", "STATE", "REPO_ID", "LFS", "NOTE")
	for _, m := range metas {
		note := "ok"
		state := "ok"
		idStr := fmt.Sprintf("%d", m.ID)
		switch m.State {
		case github.MetaMissing:
			state = "missing"
			idStr = "-"
			note = "404 Not Found on GitHub (local backup if any will be retained)"
			hasIssues = true
		case github.MetaError:
			state = "error"
			idStr = "-"
			note = redactor.Redact(fmt.Sprint(m.Err))
			hasIssues = true
		case github.MetaNotModified:
			state = "ok(304)"
		}
		if m.Renamed {
			note = fmt.Sprintf("renamed on GitHub -> %s (update config.yaml)", m.FullName)
		}
		if m.ID > 0 {
			activeIDs[ledger.RepoIDKey(m.ID)] = struct{}{}
		}
		if m.FullName != "" {
			activeNames[strings.ToLower(m.FullName)] = struct{}{}
		}
		activeNames[m.Target.CanonicalKey] = struct{}{}

		fmt.Fprintf(out, "%-30s %-12s %-10s %-8s %s\n", m.Target.Repo, state, idStr, string(m.Target.EnableLFS), note)
	}

	var willBecomeUnmanaged []string
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
			willBecomeUnmanaged = append(willBecomeUnmanaged, rec.FullName)
		}
	}

	if len(willBecomeUnmanaged) > 0 {
		fmt.Fprintf(out, "\n[!] Repositories that will become UNMANAGED (policy=%s):\n", cfg.Defaults.UnmanagedPolicy)
		for _, name := range willBecomeUnmanaged {
			fmt.Fprintf(out, "  - %s\n", name)
		}
	}

	if hasIssues {
		return ExitPartial
	}
	return ExitOK
}
