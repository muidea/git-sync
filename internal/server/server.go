package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muidea/git-backup/internal/app"
	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/ledger"
	"github.com/muidea/git-backup/internal/notify"
	"github.com/muidea/git-backup/internal/webui"
)

// Server hosts the REST API, embedded Web UI, Prometheus metrics, and built-in scheduler.
type Server struct {
	cfg      *config.Config
	logger   *slog.Logger
	hooks    *app.Hooks
	guard    *AuthGuard
	jobs     *JobManager
	redactor *auth.Redactor

	targetsMu  sync.Mutex
	schedMu    sync.RWMutex
	nextSyncAt *time.Time
}

// New constructs a Server after validating the loopback-only no-auth security policy.
func New(cfg *config.Config, logger *slog.Logger, hooks *app.Hooks) (*Server, error) {
	redactor := auth.NewRedactor()
	apiToken := strings.TrimSpace(os.Getenv(cfg.Server.APITokenEnv))
	redactor.Add(apiToken)
	redactor.AddURL(cfg.Network.ProxyURL, false)

	guard, err := NewAuthGuard(cfg.Server.Listen, apiToken)
	if err != nil {
		return nil, err
	}
	if !guard.AuthRequired() {
		logger.Warn("API/Web UI running without authentication on loopback address", "listen", cfg.Server.Listen)
	}

	// Ensure targets.json is initialized/migrated at server startup.
	if _, _, err := ledger.LoadOrMigrateTargets(cfg.Storage.RootDir, cfg.Targets); err != nil {
		return nil, fmt.Errorf("initialize targets.json: %w", err)
	}

	return &Server{
		cfg:      cfg,
		logger:   logger,
		hooks:    hooks,
		guard:    guard,
		jobs:     NewJobManager(cfg, logger, hooks),
		redactor: redactor,
	}, nil
}

// Handler returns the configured HTTP handler (also convenient for httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.guard.RequireAuth(s.handleMetrics))

	mux.HandleFunc("GET /api/v1/session", s.guard.HandleSessionGet)
	mux.HandleFunc("POST /api/v1/session", s.guard.HandleSessionPost)
	mux.HandleFunc("DELETE /api/v1/session", s.guard.HandleSessionDelete)

	mux.HandleFunc("GET /api/v1/status", s.guard.RequireAuth(s.handleStatus))
	mux.HandleFunc("GET /api/v1/targets", s.guard.RequireAuth(s.handleTargetsList))
	mux.HandleFunc("POST /api/v1/targets", s.guard.RequireAuth(s.handleTargetsAdd))
	mux.HandleFunc("POST /api/v1/targets/check", s.guard.RequireAuth(s.handleTargetsCheck))
	mux.HandleFunc("PATCH /api/v1/targets/{owner}/{repo}", s.guard.RequireAuth(s.handleTargetPatch))
	mux.HandleFunc("DELETE /api/v1/targets/{owner}/{repo}", s.guard.RequireAuth(s.handleTargetDelete))
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}", s.guard.RequireAuth(s.handleRepoDetail))

	mux.HandleFunc("POST /api/v1/sync", s.guard.RequireAuth(s.handleSyncTrigger))
	mux.HandleFunc("POST /api/v1/verify", s.guard.RequireAuth(s.handleVerifyTrigger))
	mux.HandleFunc("GET /api/v1/jobs", s.guard.RequireAuth(s.handleJobsList))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.guard.RequireAuth(s.handleJobGet))

	uiEnabled := s.cfg.Server.UIEnabled == nil || *s.cfg.Server.UIEnabled
	if uiEnabled {
		subFS, err := fs.Sub(webui.Assets, "assets")
		if err == nil {
			fileServer := http.FileServer(http.FS(subFS))
			mux.Handle("GET /assets/", http.StripPrefix("/assets/", fileServer))
			mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					http.NotFound(w, r)
					return
				}
				if !s.guard.AuthRequired() && !isLoopbackRequest(r) {
					writeJSONError(w, http.StatusForbidden, "loopback-only no-auth mode rejected non-loopback request")
					return
				}
				raw, readErr := fs.ReadFile(subFS, "index.html")
				if readErr != nil {
					http.Error(w, "UI unavailable", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write(raw)
			})
		}
	}

	return mux
}

// Run starts the HTTP server and built-in scheduler until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", s.cfg.Server.Listen, err)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	schedCtx, cancelSched := context.WithCancel(ctx)
	defer cancelSched()
	go s.runScheduler(schedCtx)

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("git-backup server started",
			"listen", ln.Addr().String(),
			"auth_required", s.guard.AuthRequired(),
			"sync_interval", s.cfg.Server.SyncInterval.Duration.String(),
		)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutting down git-backup server...")
		cancelSched()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		s.jobs.Wait()
		return nil
	case srvErr := <-errCh:
		cancelSched()
		return srvErr
	}
}

func (s *Server) runScheduler(ctx context.Context) {
	interval := s.cfg.Server.SyncInterval.Duration
	var syncTicker *time.Ticker
	var syncCh <-chan time.Time
	if interval > 0 {
		next := time.Now().UTC().Add(interval)
		s.setNextSyncAt(&next)
		syncTicker = time.NewTicker(interval)
		syncCh = syncTicker.C
		defer syncTicker.Stop()
	}

	weeklySpec := strings.TrimSpace(s.cfg.Server.WeeklyReportCron)
	var weeklyWD time.Weekday
	var weeklyHour, weeklyMin int
	var hasWeekly bool
	if weeklySpec != "" {
		if wd, hh, mm, err := config.ParseWeeklyCron(weeklySpec); err == nil {
			weeklyWD, weeklyHour, weeklyMin = wd, hh, mm
			hasWeekly = true
		}
	}

	var weeklyTicker *time.Ticker
	var weeklyCh <-chan time.Time
	lastSentWeek := ""
	if hasWeekly {
		weeklyTicker = time.NewTicker(30 * time.Second)
		weeklyCh = weeklyTicker.C
		defer weeklyTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-syncCh:
			next := time.Now().UTC().Add(interval)
			s.setNextSyncAt(&next)
			if _, _, err := s.jobs.StartSync(ctx, "scheduled", nil); err != nil {
				s.logger.Warn("skipped scheduled sync because another job is active")
			}
		case now := <-weeklyCh:
			if now.Weekday() == weeklyWD && now.Hour() == weeklyHour && now.Minute() == weeklyMin {
				year, week := now.ISOWeek()
				tag := fmt.Sprintf("%04d-W%02d", year, week)
				if tag != lastSentWeek {
					lastSentWeek = tag
					go func() {
						_ = app.RunReport(ctx, s.cfg, nil, s.hooks)
					}()
				}
			}
		}
	}
}

func (s *Server) setNextSyncAt(t *time.Time) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.nextSyncAt = t
}

func (s *Server) getNextSyncAt() *time.Time {
	s.schedMu.RLock()
	defer s.schedMu.RUnlock()
	if s.nextSyncAt == nil {
		return nil
	}
	cp := *s.nextSyncAt
	return &cp
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	repLedger, _ := ledger.LoadLedger(s.cfg.Storage.RootDir)
	summary, _ := ledger.LoadSummary(s.cfg.Storage.RootDir)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(notify.FormatPrometheus(summary, repLedger))
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	repLedger, err := ledger.LoadLedger(s.cfg.Storage.RootDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary, sumErr := ledger.LoadSummary(s.cfg.Storage.RootDir)
	if sumErr != nil && !errors.Is(sumErr, os.ErrNotExist) {
		writeJSONError(w, http.StatusInternalServerError, sumErr.Error())
		return
	}

	var totalStorage int64
	for _, rec := range repLedger.Repositories {
		totalStorage += rec.DiskSizeBytes
	}

	disk, _ := ledger.GetDiskInfo(s.cfg.Storage.RootDir)

	writeJSON(w, http.StatusOK, map[string]any{
		"root_dir":            s.cfg.Storage.RootDir,
		"summary":             summary,
		"disk":                disk,
		"total_storage_bytes": totalStorage,
		"next_sync_at":        s.getNextSyncAt(),
		"active_job":          s.jobs.Active(),
		"config_summary":      s.redactedConfigSummary(),
	})
}

func (s *Server) redactedConfigSummary() map[string]any {
	proxyDisplay := ""
	if s.cfg.Network.ProxyURL != "" {
		if u, err := url.Parse(s.cfg.Network.ProxyURL); err == nil {
			u.User = nil
			proxyDisplay = u.String()
		} else {
			proxyDisplay = "[CONFIGURED]"
		}
	}
	return map[string]any{
		"storage": map[string]any{
			"root_dir":              s.cfg.Storage.RootDir,
			"trash_retention_days":  s.cfg.Storage.TrashRetentionDays,
			"gc_interval_syncs":     s.cfg.Storage.GCIntervalSyncs,
			"min_free_disk_percent": s.cfg.Storage.MinFreeDiskPercent,
			"min_free_disk_gb":      s.cfg.Storage.MinFreeDiskGB,
		},
		"auth": map[string]any{
			"mode": s.cfg.Auth.Mode,
		},
		"defaults": map[string]any{
			"enable_lfs":       s.cfg.Defaults.EnableLFS,
			"mirror_pr_refs":   s.cfg.Defaults.MirrorPRRefs,
			"clone_timeout":    s.cfg.Defaults.CloneTimeout.Duration.String(),
			"update_timeout":   s.cfg.Defaults.UpdateTimeout.Duration.String(),
			"unmanaged_policy": s.cfg.Defaults.UnmanagedPolicy,
		},
		"sync": map[string]any{
			"concurrency":          s.cfg.Sync.Concurrency,
			"skip_unchanged":       s.cfg.Sync.SkipUnchanged,
			"force_full_sync_days": s.cfg.Sync.ForceFullSyncDays,
			"max_retries":          s.cfg.Sync.MaxRetries,
		},
		"network": map[string]any{
			"proxy_url":       proxyDisplay,
			"http_version":    s.cfg.Network.HTTPVersion,
			"low_speed_limit": s.cfg.Network.LowSpeedLimit,
			"low_speed_time":  s.cfg.Network.LowSpeedTime,
		},
		"server": map[string]any{
			"listen":             s.cfg.Server.Listen,
			"auth_required":      s.guard.AuthRequired(),
			"sync_interval":      s.cfg.Server.SyncInterval.Duration.String(),
			"weekly_report_cron": s.cfg.Server.WeeklyReportCron,
		},
	}
}

func (s *Server) handleTargetsList(w http.ResponseWriter, r *http.Request) {
	s.targetsMu.Lock()
	tf, _, err := ledger.LoadOrMigrateTargets(s.cfg.Storage.RootDir, s.cfg.Targets)
	s.targetsMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	repLedger, err := ledger.LoadLedger(s.cfg.Storage.RootDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type effectiveView struct {
		EnableLFS     config.LFSMode `json:"enable_lfs"`
		MirrorPRRefs  bool           `json:"mirror_pr_refs"`
		SkipUnchanged bool           `json:"skip_unchanged"`
		CloneTimeout  string         `json:"clone_timeout"`
		UpdateTimeout string         `json:"update_timeout"`
	}
	type targetView struct {
		Repo                string               `json:"repo"`
		ID                  int64                `json:"id,omitempty"`
		State               ledger.RepoStatus    `json:"state"`
		HeadCommit          string               `json:"head_commit,omitempty"`
		LastGitSyncAt       *time.Time           `json:"last_git_sync_at,omitempty"`
		LastCheckedAt       *time.Time           `json:"last_checked_at,omitempty"`
		DiskSizeBytes       int64                `json:"disk_size_bytes"`
		ConsecutiveFailures int                  `json:"consecutive_failures"`
		LastError           string               `json:"last_error,omitempty"`
		Overrides           *config.TargetConfig `json:"overrides,omitempty"`
		Effective           effectiveView        `json:"effective"`
	}

	resolved := s.cfg.ResolveTargetList(tf.Targets)
	activeNames := make(map[string]struct{}, len(resolved))
	activeIDs := make(map[int64]struct{}, len(resolved))
	items := make([]targetView, 0, len(resolved))

	for i, rt := range resolved {
		activeNames[rt.CanonicalKey] = struct{}{}
		ov := tf.Targets[i]
		tv := targetView{
			Repo:      rt.Repo,
			State:     "pending",
			Overrides: &ov,
			Effective: effectiveView{
				EnableLFS:     rt.EnableLFS,
				MirrorPRRefs:  rt.MirrorPRRefs,
				SkipUnchanged: rt.SkipUnchanged,
				CloneTimeout:  rt.CloneTimeout.String(),
				UpdateTimeout: rt.UpdateTimeout.String(),
			},
		}
		if rec := repLedger.FindByFullName(rt.Repo); rec != nil {
			activeIDs[rec.ID] = struct{}{}
			tv.ID = rec.ID
			tv.Repo = rec.FullName
			tv.State = rec.State
			tv.HeadCommit = rec.HeadCommit
			tv.LastGitSyncAt = rec.LastGitSyncAt
			tv.LastCheckedAt = rec.LastCheckedAt
			tv.DiskSizeBytes = rec.DiskSizeBytes
			tv.ConsecutiveFailures = rec.ConsecutiveFailures
			tv.LastError = rec.LastError
		}
		items = append(items, tv)
	}

	// Append unmanaged records from ledger for visibility in Targets tab.
	for _, rec := range repLedger.Repositories {
		if rec.State != ledger.StatusUnmanaged {
			continue
		}
		if _, ok := activeIDs[rec.ID]; ok {
			continue
		}
		if _, ok := activeNames[strings.ToLower(rec.FullName)]; ok {
			continue
		}
		items = append(items, targetView{
			Repo:                rec.FullName,
			ID:                  rec.ID,
			State:               rec.State,
			HeadCommit:          rec.HeadCommit,
			LastGitSyncAt:       rec.LastGitSyncAt,
			LastCheckedAt:       rec.LastCheckedAt,
			DiskSizeBytes:       rec.DiskSizeBytes,
			ConsecutiveFailures: rec.ConsecutiveFailures,
			LastError:           rec.LastError,
		})
	}

	stateFilter := strings.TrimSpace(r.URL.Query().Get("state"))
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if stateFilter != "" || q != "" {
		filtered := items[:0]
		for _, it := range items {
			if stateFilter != "" && string(it.State) != stateFilter {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(it.Repo), q) {
				continue
			}
			filtered = append(filtered, it)
		}
		items = filtered
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Repo < items[j].Repo })
	writeJSON(w, http.StatusOK, map[string]any{"targets": items})
}

func (s *Server) handleTargetsAdd(w http.ResponseWriter, r *http.Request) {
	var tc config.TargetConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&tc); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
		return
	}
	norm, err := config.NormalizeRepoName(tc.Repo)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	tc.Repo = norm

	if r.URL.Query().Get("check") == "true" {
		report, code, checkErr := app.CheckTargets(r.Context(), s.cfg, []config.TargetConfig{tc}, s.hooks)
		if checkErr != nil {
			writeJSONError(w, http.StatusBadGateway, checkErr.Error())
			return
		}
		if code != app.ExitOK || len(report.Items) == 0 || report.Items[0].State == "missing" || report.Items[0].State == "error" {
			note := "remote repository check failed"
			if len(report.Items) > 0 {
				note = report.Items[0].Note
			}
			writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("preflight failed for %s: %s", tc.Repo, note))
			return
		}
		if report.Items[0].Canonical != "" {
			tc.Repo = report.Items[0].Canonical
		}
	}

	s.targetsMu.Lock()
	lock, lockErr := ledger.AcquireLock(s.cfg.Storage.RootDir)
	if lockErr != nil {
		s.targetsMu.Unlock()
		if errors.Is(lockErr, ledger.ErrLockBusy) {
			writeJSONError(w, http.StatusConflict, "another sync operation holds .backup.lock; retry shortly")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, lockErr.Error())
		return
	}

	tf, _, err := ledger.LoadOrMigrateTargets(s.cfg.Storage.RootDir, s.cfg.Targets)
	if err == nil {
		err = tf.Add(tc)
		if err == nil {
			err = ledger.SaveTargets(s.cfg.Storage.RootDir, tf)
		}
	}
	_ = lock.Release()
	s.targetsMu.Unlock()

	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSONError(w, status, err.Error())
		return
	}

	var triggeredJob *JobSnapshot
	if r.URL.Query().Get("sync_now") == "true" {
		j, _, jErr := s.jobs.StartSync(context.Background(), "add_sync", []string{tc.Repo})
		if jErr == nil {
			triggeredJob = &j
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"target": tc,
		"job":    triggeredJob,
	})
}

func (s *Server) handleTargetPatch(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("owner") + "/" + r.PathValue("repo")
	var patch ledger.TargetPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&patch); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
		return
	}

	s.targetsMu.Lock()
	lock, lockErr := ledger.AcquireLock(s.cfg.Storage.RootDir)
	if lockErr != nil {
		s.targetsMu.Unlock()
		if errors.Is(lockErr, ledger.ErrLockBusy) {
			writeJSONError(w, http.StatusConflict, "another sync operation holds .backup.lock; retry shortly")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, lockErr.Error())
		return
	}

	var updated config.TargetConfig
	tf, _, err := ledger.LoadOrMigrateTargets(s.cfg.Storage.RootDir, s.cfg.Targets)
	if err == nil {
		updated, err = tf.Update(repoName, patch)
		if err == nil {
			err = ledger.SaveTargets(s.cfg.Storage.RootDir, tf)
		}
	}
	_ = lock.Release()
	s.targetsMu.Unlock()

	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"target": updated})
}

func (s *Server) handleTargetDelete(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("owner") + "/" + r.PathValue("repo")

	s.targetsMu.Lock()
	lock, lockErr := ledger.AcquireLock(s.cfg.Storage.RootDir)
	if lockErr != nil {
		s.targetsMu.Unlock()
		if errors.Is(lockErr, ledger.ErrLockBusy) {
			writeJSONError(w, http.StatusConflict, "another sync operation holds .backup.lock; retry shortly")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, lockErr.Error())
		return
	}

	tf, _, err := ledger.LoadOrMigrateTargets(s.cfg.Storage.RootDir, s.cfg.Targets)
	var canonical string
	var removed bool
	if err == nil {
		canonical, removed, err = tf.Remove(repoName)
		if err == nil && removed {
			err = ledger.SaveTargets(s.cfg.Storage.RootDir, tf)
			if err == nil {
				if repLedger, lErr := ledger.LoadLedger(s.cfg.Storage.RootDir); lErr == nil {
					if rec := repLedger.FindByFullName(canonical); rec != nil {
						rec.State = ledger.StatusUnmanaged
						rec.LastError = ""
						repLedger.UpdatedAt = time.Now().UTC()
						_ = ledger.SaveLedger(s.cfg.Storage.RootDir, repLedger)
					}
				}
			}
		}
	}
	_ = lock.Release()
	s.targetsMu.Unlock()

	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("repository %q not found in targets", repoName))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"removed":          canonical,
		"unmanaged_policy": s.cfg.Defaults.UnmanagedPolicy,
	})
}

func (s *Server) handleTargetsCheck(w http.ResponseWriter, r *http.Request) {
	s.targetsMu.Lock()
	tf, _, err := ledger.LoadOrMigrateTargets(s.cfg.Storage.RootDir, s.cfg.Targets)
	s.targetsMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	report, _, err := app.CheckTargets(r.Context(), s.cfg, tf.Targets, s.hooks)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleRepoDetail(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("owner") + "/" + r.PathValue("repo")
	repLedger, err := ledger.LoadLedger(s.cfg.Storage.RootDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec := repLedger.FindByFullName(repoName)
	if rec == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("repository %q not found in ledger", repoName))
		return
	}

	absPath := filepath.Join(s.cfg.Storage.RootDir, filepath.FromSlash(rec.LocalPath))
	var refs []string
	if _, statErr := os.Stat(absPath); statErr == nil {
		cmd := exec.CommandContext(r.Context(), "git", "-C", absPath, "show-ref")
		if out, cmdErr := cmd.Output(); cmdErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line != "" {
					refs = append(refs, line)
					if len(refs) >= 50 {
						break
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"record":       rec,
		"refs_preview": refs,
	})
}

func (s *Server) handleSyncTrigger(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repos []string `json:"repos"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
			return
		}
	}

	job, active, err := s.jobs.StartSync(context.Background(), "manual", req.Repos)
	if err != nil {
		if errors.Is(err, ErrJobBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      err.Error(),
				"active_job": active,
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleVerifyTrigger(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repos []string `json:"repos"`
		Fsck  bool     `json:"fsck"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
			return
		}
	}

	job, active, err := s.jobs.StartVerify(context.Background(), "manual", req.Repos, req.Fsck)
	if err != nil {
		if errors.Is(err, ErrJobBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      err.Error(),
				"active_job": active,
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleJobsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobs.List()})
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.jobs.Get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
