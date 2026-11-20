package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
	"github.com/muidea/git-backup/internal/gitops"
	"github.com/muidea/git-backup/internal/ledger"
)

type VerifyOptions struct {
	RepoFilter  string
	RepoFilters []string
	RunFsck     bool
	JSON        bool
	ProgressFn  func(completed, total int, currentRepo string)
}

// RunVerify compares live remote `git ls-remote` refs against local `git show-ref` for reachable targets.
func RunVerify(ctx context.Context, cfg *config.Config, opts VerifyOptions, out io.Writer, hooks *Hooks) int {
	_, code := RunVerifyWithResults(ctx, cfg, opts, out, hooks)
	return code
}

// RunVerifyWithResults executes verification and returns structured per-repo outcomes plus exit code.
func RunVerifyWithResults(ctx context.Context, cfg *config.Config, opts VerifyOptions, out io.Writer, hooks *Hooks) ([]gitops.VerifyResult, int) {
	redactor := auth.NewRedactor()
	tp, err := auth.NewTokenProvider(cfg.Auth, cfg.Network.ProxyURL, redactor)
	if err != nil {
		if out != nil {
			fmt.Fprintf(os.Stderr, "ERROR: auth init failed: %v\n", err)
		}
		return nil, ExitFatal
	}
	if _, err := tp.Token(ctx); err != nil {
		if out != nil {
			fmt.Fprintf(os.Stderr, "ERROR: obtain auth token failed: %v\n", err)
		}
		return nil, ExitFatal
	}

	repLedger, err := ledger.LoadLedger(cfg.Storage.RootDir)
	if err != nil {
		if out != nil {
			fmt.Fprintf(os.Stderr, "ERROR: load repo_ledger.json failed: %v\n", err)
		}
		return nil, ExitFatal
	}

	tf, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets)
	if err != nil {
		if out != nil {
			fmt.Fprintf(os.Stderr, "ERROR: load targets.json failed: %v\n", err)
		}
		return nil, ExitFatal
	}
	targets := cfg.ResolveTargetList(tf.Targets)

	type verifyJob struct {
		target   config.ResolvedTarget
		repoDir  string
		cloneURL string
	}

	filterSet := make(map[string]struct{})
	if fk := strings.ToLower(strings.TrimSpace(opts.RepoFilter)); fk != "" {
		filterSet[fk] = struct{}{}
	}
	for _, f := range opts.RepoFilters {
		if fk := strings.ToLower(strings.TrimSpace(f)); fk != "" {
			filterSet[fk] = struct{}{}
		}
	}
	hasFilter := len(filterSet) > 0

	var jobsList []verifyJob
	for _, t := range targets {
		rec := repLedger.FindByFullName(t.Repo)
		canonicalName := t.Repo
		if rec != nil {
			canonicalName = rec.FullName
		}
		if hasFilter {
			_, m1 := filterSet[t.CanonicalKey]
			_, m2 := filterSet[strings.ToLower(canonicalName)]
			if !m1 && !m2 {
				continue
			}
		}
		// Automatically skip missing and unmanaged entries during full verify.
		if rec != nil && (rec.State == ledger.StatusMissing || rec.State == ledger.StatusUnmanaged) {
			if hasFilter && out != nil {
				fmt.Fprintf(os.Stderr, "WARNING: repository %s is currently %s, skipping verify\n", canonicalName, rec.State)
			}
			continue
		}

		relPath := ledger.RelativeRepoPath(canonicalName)
		if rec != nil && rec.LocalPath != "" {
			relPath = rec.LocalPath
		}
		absPath := filepath.Join(cfg.Storage.RootDir, filepath.FromSlash(relPath))
		meta := github.RepoMeta{
			Target:   t,
			FullName: canonicalName,
			CloneURL: "https://github.com/" + canonicalName + ".git",
		}
		jobsList = append(jobsList, verifyJob{
			target:   t,
			repoDir:  absPath,
			cloneURL: cloneURLForMeta(meta, hooks),
		})
	}

	if hasFilter && len(jobsList) == 0 {
		if out != nil {
			fmt.Fprintf(os.Stderr, "ERROR: matching repository not found in active reachable targets\n")
		}
		return nil, ExitFatal
	}

	results := make([]gitops.VerifyResult, len(jobsList))
	ch := make(chan int, len(jobsList))
	for i := range jobsList {
		ch <- i
	}
	close(ch)

	workers := cfg.Sync.Concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobsList) && len(jobsList) > 0 {
		workers = len(jobsList)
	}

	if opts.ProgressFn != nil {
		opts.ProgressFn(0, len(jobsList), "")
	}

	var doneCount atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				job := jobsList[idx]
				if _, statErr := os.Stat(job.repoDir); statErr != nil {
					results[idx] = gitops.VerifyResult{
						Repo:  job.target.Repo,
						OK:    false,
						Error: fmt.Sprintf("local mirror missing at %s", job.repoDir),
					}
				} else {
					tok, tokErr := tp.Token(ctx)
					if tokErr != nil {
						results[idx] = gitops.VerifyResult{
							Repo:  job.target.Repo,
							OK:    false,
							Error: tokErr.Error(),
						}
					} else {
						env := auth.GitEnv(os.Environ(), tok, cfg.Network)
						jobCtx, cancel := context.WithTimeout(ctx, job.target.UpdateTimeout)
						res := gitops.VerifyRefs(jobCtx, job.repoDir, job.target.Repo, job.cloneURL, job.target.MirrorPRRefs, opts.RunFsck, env)
						cancel()
						res.Error = redactor.Redact(res.Error)
						results[idx] = res
					}
				}
				n := int(doneCount.Add(1))
				if opts.ProgressFn != nil {
					opts.ProgressFn(n, len(jobsList), job.target.Repo)
				}
			}
		}()
	}
	wg.Wait()

	allOK := true
	for _, r := range results {
		if !r.OK {
			allOK = false
			break
		}
	}

	if out != nil {
		if opts.JSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(results); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: encode verify json: %v\n", err)
				return results, ExitFatal
			}
		} else {
			fmt.Fprintf(out, "=== Reference Integrity Verification (%d repositories) ===\n", len(results))
			for _, r := range results {
				if r.OK {
					fmt.Fprintf(out, "[OK]   %s\n", r.Repo)
					continue
				}
				fmt.Fprintf(out, "[FAIL] %s", r.Repo)
				if r.Error != "" {
					fmt.Fprintf(out, " (%s)", r.Error)
				}
				fmt.Fprintln(out)
				for _, m := range r.Mismatches {
					fmt.Fprintf(out, "       - %s: %s (remote=%s local=%s)\n", m.Kind, m.Ref, m.RemoteSHA, m.LocalSHA)
				}
			}
		}
	}

	if !allOK {
		return results, ExitPartial
	}
	return results, ExitOK
}
