package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
	"github.com/muidea/git-backup/internal/gitops"
	"github.com/muidea/git-backup/internal/ledger"
	"github.com/muidea/git-backup/internal/notify"
)

type syncTask struct {
	meta         github.RepoMeta
	ledgerKey    string
	record       *ledger.RepoRecord
	isNewClone   bool
	localPathRel string
	localPathAbs string
	cloneURL     string
}

type syncTaskOutcome struct {
	task       syncTask
	finalState ledger.RepoStatus
	headCommit string
	diskBytes  int64
	lfsEnabled bool
	gcRan      bool
	errMsg     string
}

// SyncOptions customizes a sync run (optional repository filter and progress callback).
type SyncOptions struct {
	RepoFilter []string
	ProgressFn func(completed, total int, currentRepo string)
}

// SyncResult captures the outcome of RunSyncWithOptions.
type SyncResult struct {
	ExitCode   int
	Summary    *ledger.StatusSummary
	FatalError string
}

// RunSync executes the end-to-end backup workflow and returns exit code 0, 1, or 2.
func RunSync(ctx context.Context, cfg *config.Config, logger *slog.Logger, hooks *Hooks) int {
	return RunSyncWithOptions(ctx, cfg, logger, SyncOptions{}, hooks).ExitCode
}

// RunSyncWithOptions executes a full or filtered sync run and returns detailed results.
func RunSyncWithOptions(ctx context.Context, cfg *config.Config, logger *slog.Logger, opts SyncOptions, hooks *Hooks) SyncResult {
	startedAt := currentTime(hooks)
	runID := startedAt.Format("20060102-150405")
	redactor := auth.NewRedactor()
	webhook := notify.NewWebhookSender(cfg.Observability.Alert, cfg.Network.ProxyURL, redactor)

	failFatal := func(msg string, err error) SyncResult {
		full := msg
		if err != nil {
			full = fmt.Sprintf("%s: %s", msg, redactor.Redact(err.Error()))
		}
		logger.Error("fatal sync error", "run_id", runID, "error", full)
		_ = webhook.Send(ctx, notify.AlertPayload{
			EventType: "fatal",
			Title:     "git-backup 致命错误中止",
			Message:   full,
			Timestamp: currentTime(hooks),
		})
		return SyncResult{ExitCode: ExitFatal, FatalError: full}
	}

	lock, err := ledger.AcquireLock(cfg.Storage.RootDir)
	if err != nil {
		if errors.Is(err, ledger.ErrLockBusy) {
			logger.Info("another git-backup instance holds .backup.lock, exiting safely", "root_dir", cfg.Storage.RootDir)
			return SyncResult{ExitCode: ExitOK}
		}
		return failFatal("acquire backup lock failed", err)
	}
	defer func() { _ = lock.Release() }()

	if hooks == nil || !hooks.SkipGitVersionCheck {
		if err := gitops.CheckGitVersion(ctx); err != nil {
			return failFatal("runtime dependency check failed", err)
		}
	}
	if hooks == nil || !hooks.SkipDiskSpaceCheck {
		if err := ledger.CheckDiskSpace(cfg.Storage.RootDir, cfg.Storage.MinFreeDiskPercent, cfg.Storage.MinFreeDiskGB); err != nil {
			return failFatal("disk space preflight failed", err)
		}
	}

	tp, err := auth.NewTokenProvider(cfg.Auth, cfg.Network.ProxyURL, redactor)
	if err != nil {
		return failFatal("initialize auth provider failed", err)
	}
	ghClient := github.NewClient(tp, cfg.Network.ProxyURL, redactor)
	if hooks != nil && hooks.GitHubClientCustomizer != nil {
		hooks.GitHubClientCustomizer(ghClient, tp)
	}
	if err := ghClient.ValidateAuth(ctx); err != nil {
		return failFatal("credential validation failed", err)
	}

	if err := ledger.CleanTmpClones(cfg.Storage.RootDir); err != nil {
		logger.Warn("failed cleaning .tmp_clones", "error", redactor.Redact(err.Error()))
	}

	repLedger, err := ledger.LoadLedger(cfg.Storage.RootDir)
	if err != nil {
		return failFatal("load repo_ledger.json failed", err)
	}

	targetsFile, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets)
	if err != nil {
		return failFatal("load targets.json failed", err)
	}
	allTargets := cfg.ResolveTargetList(targetsFile.Targets)

	targets := allTargets
	isFiltered := len(opts.RepoFilter) > 0
	if isFiltered {
		var filterErr error
		targets, filterErr = filterResolvedTargets(allTargets, opts.RepoFilter)
		if filterErr != nil {
			return failFatal("invalid repository filter", filterErr)
		}
	}

	metas, err := ghClient.FetchMetadata(ctx, targets, repLedger, cfg.Sync.Concurrency, cfg.Sync.MaxRetries)
	if err != nil {
		return failFatal("fetch GitHub metadata failed", err)
	}

	now := currentTime(hooks)
	activeKeys := make(map[string]struct{}, len(metas))
	activeNames := make(map[string]struct{}, len(metas))
	for _, t := range allTargets {
		activeNames[t.CanonicalKey] = struct{}{}
	}
	for _, m := range metas {
		if m.ID > 0 {
			activeKeys[ledger.RepoIDKey(m.ID)] = struct{}{}
		}
		if m.FullName != "" {
			activeNames[strings.ToLower(m.FullName)] = struct{}{}
		}
	}

	// 1. Reconcile unmanaged repositories only on full sync runs.
	if !isFiltered {
		reconcileUnmanaged(cfg, repLedger, activeKeys, activeNames, now, logger)
	}

	// 2. Reconcile whitelisted repositories into skip, missing, failed, or worker tasks.
	var tasks []syncTask
	var clonedCount, updatedCount, upToDateCount, failedCount, missingCount int
	var newlyAlerted, recovered []string

	for _, meta := range metas {
		if meta.State == github.MetaMissing {
			handleMissingMeta(cfg, repLedger, meta, now, logger, &missingCount)
			continue
		}
		if meta.State == github.MetaError {
			handleAPIErrorMeta(cfg, repLedger, meta, now, redactor, &failedCount)
			continue
		}

		if meta.Renamed {
			logger.Warn("repository renamed on GitHub; please update targets in config.yaml",
				"configured", meta.Target.Repo,
				"canonical", meta.FullName,
				"repo_id", meta.ID,
			)
		}

		key := ledger.RepoIDKey(meta.ID)
		// Migrate synthetic missing placeholder key if this repo previously 404'd before ever resolving an ID.
		missingKey := "missing:" + meta.Target.CanonicalKey
		if _, hasSynthetic := repLedger.Repositories[missingKey]; hasSynthetic {
			if _, hasReal := repLedger.Repositories[key]; !hasReal {
				repLedger.Repositories[key] = repLedger.Repositories[missingKey]
				repLedger.Repositories[key].ID = meta.ID
			}
			delete(repLedger.Repositories, missingKey)
		}

		rec := repLedger.Repositories[key]
		destRel := ledger.RelativeRepoPath(meta.FullName)
		destAbs := filepath.Join(cfg.Storage.RootDir, filepath.FromSlash(destRel))
		cloneURL := cloneURLForMeta(meta, hooks)

		// Handle rename migration when same repo_id moved to a new path.
		if rec != nil && filepath.Clean(rec.LocalPath) != filepath.Clean(destRel) {
			oldAbs := filepath.Join(cfg.Storage.RootDir, filepath.FromSlash(rec.LocalPath))
			if _, statErr := os.Stat(oldAbs); statErr == nil && !strings.HasPrefix(filepath.ToSlash(rec.LocalPath), ".trash/") {
				if conflictRec := repLedger.FindByLocalPath(destRel); conflictRec != nil && conflictRec.ID != meta.ID {
					isolateConflictPath(cfg.Storage.RootDir, repLedger, conflictRec, destAbs, now, logger)
				} else if _, destStat := os.Stat(destAbs); destStat == nil {
					if !isSamePathCaseInsensitive(oldAbs, destAbs) {
						isolateRawConflictDir(cfg.Storage.RootDir, destAbs, meta.FullName, "rename destination already exists", now, logger)
					}
				}
				if mkErr := os.MkdirAll(filepath.Dir(destAbs), 0o755); mkErr != nil {
					logger.Error("mkdir for rename failed", "repo", meta.FullName, "error", mkErr)
				} else if mvErr := os.Rename(oldAbs, destAbs); mvErr != nil {
					logger.Error("atomic rename migration failed", "from", oldAbs, "to", destAbs, "error", mvErr)
				} else {
					_ = gitops.SetRemoteURL(ctx, destAbs, cloneURL)
					rec.LocalPath = destRel
					rec.FullName = meta.FullName
					rec.CloneURL = cloneURL
					logger.Info("migrated renamed repository", "repo_id", meta.ID, "new_path", destRel)
				}
			}
		}

		// Check cold-start adoption or path collision when rec is nil or local dir is missing.
		isNewClone := false
		if rec == nil {
			if _, statErr := os.Stat(destAbs); statErr == nil {
				if occupant := repLedger.FindByLocalPath(destRel); occupant != nil && occupant.ID != meta.ID {
					isolateConflictPath(cfg.Storage.RootDir, repLedger, occupant, destAbs, now, logger)
					isNewClone = true
				} else if ok, reason := gitops.CheckBareMirrorFor(ctx, destAbs, meta.FullName); ok {
					logger.Info("adopting existing bare repository into ledger", "repo", meta.FullName, "repo_id", meta.ID)
					_ = gitops.SetRemoteURL(ctx, destAbs, cloneURL)
					rec = &ledger.RepoRecord{
						ID:        meta.ID,
						FullName:  meta.FullName,
						LocalPath: destRel,
						CloneURL:  cloneURL,
						State:     ledger.StatusUpdated,
					}
					repLedger.Repositories[key] = rec
				} else {
					isolateRawConflictDir(cfg.Storage.RootDir, destAbs, meta.FullName, reason, now, logger)
					isNewClone = true
				}
			} else {
				isNewClone = true
			}
		} else {
			if _, statErr := os.Stat(destAbs); errors.Is(statErr, os.ErrNotExist) {
				isNewClone = true
			}
		}

		if rec == nil {
			rec = &ledger.RepoRecord{
				ID:        meta.ID,
				FullName:  meta.FullName,
				LocalPath: destRel,
				CloneURL:  cloneURL,
			}
			repLedger.Repositories[key] = rec
		} else {
			rec.ID = meta.ID
			rec.FullName = meta.FullName
			rec.LocalPath = destRel
			rec.CloneURL = cloneURL
		}

		if !isNewClone && shouldSkipUnchanged(meta, rec, cfg.Sync.ForceFullSyncDays, now) {
			wasAlerted := rec.LastAlertAt != nil && !rec.LastAlertAt.IsZero()
			rec.State = ledger.StatusUpToDate
			rec.LastCheckedAt = &now
			rec.ConsecutiveFailures = 0
			rec.LastError = ""
			upToDateCount++
			if wasAlerted {
				rec.LastAlertAt = nil
				recovered = append(recovered, rec.FullName)
			}
			continue
		}

		tasks = append(tasks, syncTask{
			meta:         meta,
			ledgerKey:    key,
			record:       rec,
			isNewClone:   isNewClone,
			localPathRel: destRel,
			localPathAbs: destAbs,
			cloneURL:     cloneURL,
		})
	}

	// 3. Execute sync tasks concurrently in bounded Worker Pool.
	preCompleted := len(metas) - len(tasks)
	if opts.ProgressFn != nil {
		opts.ProgressFn(preCompleted, len(metas), "")
	}
	outcomes := runWorkerPool(ctx, cfg, tp, redactor, tasks, logger, func(done int, repo string) {
		if opts.ProgressFn != nil {
			opts.ProgressFn(preCompleted+done, len(metas), repo)
		}
	})
	for _, oc := range outcomes {
		rec := oc.task.record
		wasAlerted := rec.LastAlertAt != nil && !rec.LastAlertAt.IsZero()
		checkedNow := currentTime(hooks)
		rec.LastCheckedAt = &checkedNow

		if oc.finalState == ledger.StatusFailed {
			failedCount++
			rec.State = ledger.StatusFailed
			rec.ConsecutiveFailures++
			rec.LastError = redactor.Redact(oc.errMsg)
			logger.Warn("repository sync failed",
				"repo", rec.FullName,
				"consecutive_failures", rec.ConsecutiveFailures,
				"error", rec.LastError,
			)
			continue
		}

		switch oc.finalState {
		case ledger.StatusCloned:
			clonedCount++
		default:
			updatedCount++
		}

		rec.State = oc.finalState
		rec.HeadCommit = oc.headCommit
		rec.DiskSizeBytes = oc.diskBytes
		rec.LFSEnabled = oc.lfsEnabled
		rec.ConsecutiveFailures = 0
		rec.LastError = ""
		rec.LastGitSyncAt = &checkedNow
		pushed := oc.task.meta.PushedAt
		if !pushed.IsZero() {
			rec.RemotePushedAt = &pushed
		}
		if oc.task.meta.ETag != "" {
			rec.ETag = oc.task.meta.ETag
		}
		if oc.gcRan {
			rec.SyncCountSinceGC = 0
		} else {
			rec.SyncCountSinceGC++
		}
		if wasAlerted {
			rec.LastAlertAt = nil
			recovered = append(recovered, rec.FullName)
		}
	}

	// 4. Reclaim expired .trash directories and archived ledger entries.
	if _, trashErr := ledger.CleanupTrash(cfg.Storage.RootDir, cfg.Storage.TrashRetentionDays, repLedger, now); trashErr != nil {
		logger.Warn("cleanup .trash failed", "error", redactor.Redact(trashErr.Error()))
	}

	// 5. Build summary & exit code.
	finishedAt := currentTime(hooks)
	unmanagedCount := 0
	var abnormal []ledger.AbnormalReport
	for _, rec := range repLedger.Repositories {
		switch rec.State {
		case ledger.StatusUnmanaged:
			unmanagedCount++
		case ledger.StatusFailed, ledger.StatusMissing:
			abnormal = append(abnormal, ledger.AbnormalReport{
				Repo:                rec.FullName,
				State:               rec.State,
				ConsecutiveFailures: rec.ConsecutiveFailures,
				LastError:           rec.LastError,
			})
		}
	}
	sort.Slice(abnormal, func(i, j int) bool { return abnormal[i].Repo < abnormal[j].Repo })

	if isFiltered {
		// Recompute global counts across all targets so status_summary.json stays globally consistent.
		clonedCount, updatedCount, upToDateCount, failedCount, missingCount = countAcrossTargets(allTargets, repLedger, metas, outcomes)
	}

	managedTotal := clonedCount + updatedCount + upToDateCount + failedCount + missingCount
	exitCode := ExitOK
	if failedCount > 0 || missingCount > 0 {
		exitCode = ExitPartial
	}

	minBulkMissing := int(math.Ceil(float64(managedTotal) * cfg.Safety.MaxMissingAlertRatio))
	if minBulkMissing < 2 {
		minBulkMissing = 2
	}
	bulkMissing := managedTotal > 0 && missingCount >= minBulkMissing && !isFiltered

	summary := &ledger.StatusSummary{
		SchemaVersion:             ledger.SchemaVersion,
		RunID:                     runID,
		StartedAt:                 startedAt,
		FinishedAt:                finishedAt,
		DurationMS:                finishedAt.Sub(startedAt).Milliseconds(),
		ExitCode:                  exitCode,
		BulkMissingAlertTriggered: bulkMissing,
		Counts: ledger.SummaryCounts{
			ManagedTotal: managedTotal,
			Cloned:       clonedCount,
			Updated:      updatedCount,
			UpToDate:     upToDateCount,
			Failed:       failedCount,
			Missing:      missingCount,
			Unmanaged:    unmanagedCount,
		},
		AbnormalRepos: abnormal,
	}

	repLedger.UpdatedAt = finishedAt
	if err := ledger.SaveLedger(cfg.Storage.RootDir, repLedger); err != nil {
		return failFatal("persist repo_ledger.json failed", err)
	}
	if err := ledger.SaveSummary(cfg.Storage.RootDir, summary); err != nil {
		return failFatal("persist status_summary.json failed", err)
	}
	if err := notify.WritePrometheus(cfg.Observability.PrometheusTextfilePath, summary, repLedger); err != nil {
		logger.Warn("write prometheus textfile failed", "path", cfg.Observability.PrometheusTextfilePath, "error", err)
	}

	// 6. Edge-triggered + cooldown Webhook alerts and recovery notifications.
	ledgerDirty := false
	for _, rec := range repLedger.Repositories {
		if rec.State != ledger.StatusFailed && rec.State != ledger.StatusMissing {
			continue
		}
		if rec.State == ledger.StatusFailed && rec.ConsecutiveFailures < cfg.Observability.Alert.FailureThreshold {
			continue
		}
		if rec.LastAlertAt == nil || finishedAt.Sub(*rec.LastAlertAt) >= cfg.Observability.Alert.AlertCooldown.Duration {
			alertTime := finishedAt
			rec.LastAlertAt = &alertTime
			ledgerDirty = true
			newlyAlerted = append(newlyAlerted, fmt.Sprintf("- `%s` [%s, fails=%d]: %s", rec.FullName, rec.State, rec.ConsecutiveFailures, rec.LastError))
		}
	}

	if bulkMissing {
		_ = webhook.Send(ctx, notify.AlertPayload{
			EventType:     "bulk_missing",
			Title:         "git-backup 批量 404/失权高危告警",
			Message:       fmt.Sprintf("本轮 %d/%d 个白名单仓库返回 404 Not Found，本地已有备份已全部原地保留，请立即核查 Token/GitHub App 访问权限。", missingCount, managedTotal),
			Timestamp:     finishedAt,
			Summary:       summary,
			AbnormalRepos: abnormal,
		})
	} else if len(newlyAlerted) > 0 {
		_ = webhook.Send(ctx, notify.AlertPayload{
			EventType:     "repo_alert",
			Title:         fmt.Sprintf("git-backup 仓库异常告警 (%d)", len(newlyAlerted)),
			Message:       strings.Join(newlyAlerted, "\n"),
			Timestamp:     finishedAt,
			Summary:       summary,
			AbnormalRepos: abnormal,
		})
	}

	if len(recovered) > 0 {
		sort.Strings(recovered)
		_ = webhook.Send(ctx, notify.AlertPayload{
			EventType: "recovery",
			Title:     fmt.Sprintf("git-backup 仓库同步已恢复 (%d)", len(recovered)),
			Message:   "以下仓库已恢复正常同步：\n- `" + strings.Join(recovered, "`\n- `") + "`",
			Timestamp: finishedAt,
			Summary:   summary,
		})
	}

	if ledgerDirty {
		_ = ledger.SaveLedger(cfg.Storage.RootDir, repLedger)
	}

	logger.Info("sync completed",
		"run_id", runID,
		"exit_code", exitCode,
		"managed_total", managedTotal,
		"cloned", clonedCount,
		"updated", updatedCount,
		"up_to_date", upToDateCount,
		"failed", failedCount,
		"missing", missingCount,
		"unmanaged", unmanagedCount,
	)
	return SyncResult{ExitCode: exitCode, Summary: summary}
}

func reconcileUnmanaged(
	cfg *config.Config,
	repLedger *ledger.RepoLedger,
	activeKeys map[string]struct{},
	activeNames map[string]struct{},
	now time.Time,
	logger *slog.Logger,
) {
	ts := now.Format("20060102_150405")
	for key, rec := range repLedger.Repositories {
		if strings.HasPrefix(key, "missing:") {
			nameKey := strings.TrimPrefix(key, "missing:")
			if _, ok := activeNames[nameKey]; !ok {
				delete(repLedger.Repositories, key)
			}
			continue
		}
		if _, ok := activeKeys[key]; ok {
			continue
		}
		if _, ok := activeNames[strings.ToLower(rec.FullName)]; ok {
			continue
		}

		rec.State = ledger.StatusUnmanaged
		rec.LastError = ""
		if cfg.Defaults.UnmanagedPolicy == "archive" && !strings.HasPrefix(filepath.ToSlash(rec.LocalPath), ".trash/") {
			srcAbs := filepath.Join(cfg.Storage.RootDir, filepath.FromSlash(rec.LocalPath))
			if _, err := os.Stat(srcAbs); err == nil {
				relTrash := filepath.ToSlash(filepath.Join(".trash", "unmanaged_"+ts, rec.FullName+".git"))
				dstAbs := filepath.Join(cfg.Storage.RootDir, filepath.FromSlash(relTrash))
				if mkErr := os.MkdirAll(filepath.Dir(dstAbs), 0o755); mkErr == nil {
					if mvErr := os.Rename(srcAbs, dstAbs); mvErr == nil {
						rec.LocalPath = relTrash
						logger.Info("archived unmanaged repository to .trash", "repo", rec.FullName, "path", relTrash)
					} else {
						logger.Warn("failed archiving unmanaged repository", "repo", rec.FullName, "error", mvErr)
					}
				}
			}
		}
	}
}

func handleMissingMeta(
	cfg *config.Config,
	repLedger *ledger.RepoLedger,
	meta github.RepoMeta,
	now time.Time,
	logger *slog.Logger,
	missingCount *int,
) {
	*missingCount++
	rec := repLedger.FindByFullName(meta.Target.Repo)
	if rec == nil {
		key := "missing:" + meta.Target.CanonicalKey
		rec = repLedger.Repositories[key]
		if rec == nil {
			rec = &ledger.RepoRecord{
				ID:        0,
				FullName:  meta.Target.Repo,
				LocalPath: ledger.RelativeRepoPath(meta.Target.Repo),
				CloneURL:  meta.CloneURL,
			}
			repLedger.Repositories[key] = rec
		}
	}
	rec.State = ledger.StatusMissing
	rec.LastCheckedAt = &now
	rec.ConsecutiveFailures++
	rec.LastError = "GitHub API returned 404 Not Found; local backup retained intact"

	absPath := filepath.Join(cfg.Storage.RootDir, filepath.FromSlash(rec.LocalPath))
	if _, err := os.Stat(absPath); err == nil {
		logger.Warn("whitelisted repository returned 404; retaining local backup intact", "repo", rec.FullName, "local_path", rec.LocalPath)
	} else {
		rec.LastError = "GitHub API returned 404 Not Found (no local backup present)"
		logger.Warn("whitelisted repository returned 404 with no local backup", "repo", rec.FullName)
	}
}

func handleAPIErrorMeta(
	cfg *config.Config,
	repLedger *ledger.RepoLedger,
	meta github.RepoMeta,
	now time.Time,
	redactor *auth.Redactor,
	failedCount *int,
) {
	*failedCount++
	rec := repLedger.FindByFullName(meta.Target.Repo)
	if rec == nil {
		key := "missing:" + meta.Target.CanonicalKey
		rec = repLedger.Repositories[key]
		if rec == nil {
			rec = &ledger.RepoRecord{
				FullName:  meta.Target.Repo,
				LocalPath: ledger.RelativeRepoPath(meta.Target.Repo),
				CloneURL:  "https://github.com/" + meta.Target.Repo + ".git",
			}
			repLedger.Repositories[key] = rec
		}
	}
	rec.State = ledger.StatusFailed
	rec.LastCheckedAt = &now
	rec.ConsecutiveFailures++
	if meta.Err != nil {
		rec.LastError = redactor.Redact(meta.Err.Error())
	} else {
		rec.LastError = "GitHub API metadata query failed"
	}
}

func shouldSkipUnchanged(meta github.RepoMeta, rec *ledger.RepoRecord, forceDays int, now time.Time) bool {
	if !meta.Target.SkipUnchanged || rec == nil {
		return false
	}
	if rec.ConsecutiveFailures > 0 {
		return false
	}
	switch rec.State {
	case ledger.StatusCloned, ledger.StatusUpdated, ledger.StatusUpToDate:
	default:
		return false
	}
	if rec.RemotePushedAt == nil || meta.PushedAt.IsZero() {
		return false
	}
	if !rec.RemotePushedAt.UTC().Equal(meta.PushedAt.UTC()) {
		return false
	}
	if rec.LastGitSyncAt == nil {
		return false
	}
	if forceDays > 0 && now.Sub(*rec.LastGitSyncAt) >= time.Duration(forceDays)*24*time.Hour {
		return false
	}
	return true
}

func filterResolvedTargets(all []config.ResolvedTarget, filter []string) ([]config.ResolvedTarget, error) {
	byKey := make(map[string]config.ResolvedTarget, len(all))
	for _, t := range all {
		byKey[t.CanonicalKey] = t
	}
	seen := make(map[string]struct{}, len(filter))
	out := make([]config.ResolvedTarget, 0, len(filter))
	for _, raw := range filter {
		norm, err := config.NormalizeRepoName(raw)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(norm)
		if _, dup := seen[key]; dup {
			continue
		}
		t, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("repository %q is not in targets whitelist", norm)
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}

func countAcrossTargets(all []config.ResolvedTarget, l *ledger.RepoLedger, runMetas []github.RepoMeta, outcomes []syncTaskOutcome) (cloned, updated, upToDate, failed, missing int) {
	runStatusByKey := make(map[string]ledger.RepoStatus, len(runMetas))
	for _, m := range runMetas {
		switch m.State {
		case github.MetaMissing:
			runStatusByKey[m.Target.CanonicalKey] = ledger.StatusMissing
		case github.MetaError:
			runStatusByKey[m.Target.CanonicalKey] = ledger.StatusFailed
		case github.MetaOK, github.MetaNotModified:
			runStatusByKey[m.Target.CanonicalKey] = ledger.StatusUpToDate
		}
	}
	for _, oc := range outcomes {
		runStatusByKey[oc.task.meta.Target.CanonicalKey] = oc.finalState
	}

	for _, t := range all {
		if st, ok := runStatusByKey[t.CanonicalKey]; ok {
			switch st {
			case ledger.StatusCloned:
				cloned++
			case ledger.StatusUpdated:
				updated++
			case ledger.StatusUpToDate:
				upToDate++
			case ledger.StatusFailed:
				failed++
			case ledger.StatusMissing:
				missing++
			}
			continue
		}
		rec := l.FindByFullName(t.Repo)
		if rec == nil {
			continue
		}
		switch rec.State {
		case ledger.StatusCloned, ledger.StatusUpdated, ledger.StatusUpToDate:
			upToDate++
		case ledger.StatusFailed:
			failed++
		case ledger.StatusMissing:
			missing++
		}
	}
	return
}

func runWorkerPool(
	ctx context.Context,
	cfg *config.Config,
	tp *auth.TokenProvider,
	redactor *auth.Redactor,
	tasks []syncTask,
	logger *slog.Logger,
	onDone func(done int, repo string),
) []syncTaskOutcome {
	if len(tasks) == 0 {
		return nil
	}
	outcomes := make([]syncTaskOutcome, len(tasks))
	jobs := make(chan int, len(tasks))
	for i := range tasks {
		jobs <- i
	}
	close(jobs)

	workers := cfg.Sync.Concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	var completed atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				outcomes[idx] = executeSyncTask(ctx, cfg, tp, redactor, tasks[idx], logger)
				n := int(completed.Add(1))
				if onDone != nil {
					onDone(n, tasks[idx].meta.Target.Repo)
				}
			}
		}()
	}
	wg.Wait()
	return outcomes
}

func executeSyncTask(
	ctx context.Context,
	cfg *config.Config,
	tp *auth.TokenProvider,
	redactor *auth.Redactor,
	task syncTask,
	logger *slog.Logger,
) syncTaskOutcome {
	out := syncTaskOutcome{task: task}
	timeout := task.meta.Target.UpdateTimeout
	if task.isNewClone {
		timeout = task.meta.Target.CloneTimeout
	}

	var lastErr error
	for attempt := 0; attempt <= cfg.Sync.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		tok, err := tp.Token(ctx)
		if err != nil {
			lastErr = err
			break
		}
		env := auth.GitEnv(os.Environ(), tok, cfg.Network)
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)

		if task.isNewClone {
			tmpDir := filepath.Join(cfg.Storage.RootDir, ".tmp_clones", filepath.FromSlash(task.meta.FullName+".git"))
			err = gitops.CloneMirrorAtomic(attemptCtx, tmpDir, task.localPathAbs, task.cloneURL, task.meta.Target.MirrorPRRefs, env)
		} else {
			if err = gitops.EnsureRefspec(attemptCtx, task.localPathAbs, task.meta.Target.MirrorPRRefs); err == nil {
				err = gitops.UpdateMirror(attemptCtx, task.localPathAbs, env)
			}
		}

		if err == nil {
			var head string
			head, err = gitops.HeadCommit(attemptCtx, task.localPathAbs)
			if err == nil {
				out.headCommit = head
				var lfsNeeded bool
				if head != "EMPTY" {
					lfsNeeded, err = gitops.ShouldFetchLFS(attemptCtx, task.localPathAbs, task.meta.Target.EnableLFS)
				}
				if err == nil && lfsNeeded {
					out.lfsEnabled = true
					err = gitops.FetchLFS(attemptCtx, task.localPathAbs, env)
				}
			}
		}
		cancel()

		if err == nil {
			if cfg.Storage.GCIntervalSyncs > 0 && task.record.SyncCountSinceGC+1 >= cfg.Storage.GCIntervalSyncs {
				gcCtx, gcCancel := context.WithTimeout(ctx, task.meta.Target.UpdateTimeout)
				if gcErr := gitops.RunMaintenance(gcCtx, task.localPathAbs); gcErr != nil {
					logger.Warn("git maintenance failed", "repo", task.meta.FullName, "error", redactor.Redact(gcErr.Error()))
				} else {
					out.gcRan = true
				}
				gcCancel()
			}
			sizeBytes, sizeErr := ledger.DirSizeBytes(task.localPathAbs)
			if sizeErr == nil {
				out.diskBytes = sizeBytes
			}
			if task.isNewClone {
				out.finalState = ledger.StatusCloned
			} else {
				out.finalState = ledger.StatusUpdated
			}
			return out
		}

		lastErr = err
		if attempt < cfg.Sync.MaxRetries && isRetryableGitError(err) {
			wait := gitRetryBackoff(attempt + 1)
			logger.Warn("retrying git sync after transient error",
				"repo", task.meta.FullName,
				"attempt", attempt+1,
				"wait", wait.String(),
				"error", redactor.Redact(err.Error()),
			)
			if sleepErr := sleepContext(ctx, wait); sleepErr != nil {
				lastErr = sleepErr
				break
			}
			continue
		}
		break
	}

	out.finalState = ledger.StatusFailed
	if lastErr != nil {
		out.errMsg = lastErr.Error()
	}
	return out
}

func isolateConflictPath(rootDir string, repLedger *ledger.RepoLedger, occupant *ledger.RepoRecord, destAbs string, now time.Time, logger *slog.Logger) {
	relTrash := filepath.ToSlash(filepath.Join(".trash", "conflict_"+now.Format("20060102_150405"), occupant.FullName+".git"))
	dstTrash := filepath.Join(rootDir, filepath.FromSlash(relTrash))
	if err := os.MkdirAll(filepath.Dir(dstTrash), 0o755); err != nil {
		logger.Warn("mkdir for conflict isolation failed", "error", err)
		return
	}
	if err := os.Rename(destAbs, dstTrash); err != nil {
		logger.Warn("rename conflict repo failed", "from", destAbs, "to", dstTrash, "error", err)
		return
	}
	occupant.LocalPath = relTrash
	logger.Warn("isolated conflicting repo_id path to .trash", "old_repo", occupant.FullName, "old_id", occupant.ID, "trash_path", relTrash)
}

func isolateRawConflictDir(rootDir, destAbs, fullName, reason string, now time.Time, logger *slog.Logger) {
	relTrash := filepath.ToSlash(filepath.Join(".trash", "conflict_"+now.Format("20060102_150405"), fullName+".git"))
	dstTrash := filepath.Join(rootDir, filepath.FromSlash(relTrash))
	if err := os.MkdirAll(filepath.Dir(dstTrash), 0o755); err != nil {
		logger.Warn("mkdir for unknown directory isolation failed", "path", destAbs, "error", err)
		return
	}
	if err := os.Rename(destAbs, dstTrash); err != nil {
		logger.Warn("isolate unknown existing directory failed", "path", destAbs, "trash_path", relTrash, "error", err)
		return
	}
	logger.Warn("isolated unknown existing directory to .trash", "path", destAbs, "trash_path", relTrash, "reason", reason)
}

func isSamePathCaseInsensitive(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) && filepath.Clean(a) != filepath.Clean(b)
}

func isRetryableGitError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "git-lfs binary is not installed") ||
		strings.Contains(low, "authentication failed") ||
		strings.Contains(low, "repository not found") {
		return false
	}
	for _, needle := range []string{
		"timed out", "timeout", "connection reset", "tls", "gnutls",
		"502", "503", "504", "rpc failed", "unexpected disconnect", "could not resolve host",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

func gitRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := 2 * time.Second * (1 << (attempt - 1))
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	jitter := time.Duration(rand.IntN(1000)) * time.Millisecond
	return base + jitter
}
