package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/muidea/git-backup/internal/app"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/gitops"
	"github.com/muidea/git-backup/internal/ledger"
)

const maxJobHistory = 50

type JobKind string

const (
	JobKindSync   JobKind = "sync"
	JobKindVerify JobKind = "verify"
)

type JobStatus string

const (
	JobQueued         JobStatus = "queued"
	JobRunning        JobStatus = "running"
	JobSucceeded      JobStatus = "succeeded"
	JobPartialFailure JobStatus = "partial_failure"
	JobFailed         JobStatus = "failed"
)

var ErrJobBusy = errors.New("another sync or verify job is already running")

// JobSnapshot represents an async sync or verify job and its progress/result.
type JobSnapshot struct {
	ID            string                `json:"id"`
	Kind          JobKind               `json:"kind"`
	Trigger       string                `json:"trigger"` // manual | scheduled | add_sync
	Repos         []string              `json:"repos,omitempty"`
	Fsck          bool                  `json:"fsck,omitempty"`
	Status        JobStatus             `json:"status"`
	Total         int                   `json:"total"`
	Completed     int                   `json:"completed"`
	CurrentRepo   string                `json:"current_repo,omitempty"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    *time.Time            `json:"finished_at,omitempty"`
	ExitCode      *int                  `json:"exit_code,omitempty"`
	Summary       *ledger.StatusSummary `json:"summary,omitempty"`
	VerifyResults []gitops.VerifyResult `json:"verify_results,omitempty"`
	Error         string                `json:"error,omitempty"`
}

// JobManager coordinates non-overlapping background sync/verify jobs.
type JobManager struct {
	cfg    *config.Config
	logger *slog.Logger
	hooks  *app.Hooks

	mu       sync.Mutex
	activeID string
	jobs     map[string]*JobSnapshot
	order    []string
	wg       sync.WaitGroup
}

func NewJobManager(cfg *config.Config, logger *slog.Logger, hooks *app.Hooks) *JobManager {
	return &JobManager{
		cfg:    cfg,
		logger: logger,
		hooks:  hooks,
		jobs:   make(map[string]*JobSnapshot),
	}
}

// StartSync launches an asynchronous sync job (full or subset) or returns ErrJobBusy.
func (m *JobManager) StartSync(ctx context.Context, trigger string, repos []string) (JobSnapshot, *JobSnapshot, error) {
	return m.startJob(ctx, JobKindSync, trigger, repos, false)
}

// StartVerify launches an asynchronous verify job or returns ErrJobBusy.
func (m *JobManager) StartVerify(ctx context.Context, trigger string, repos []string, fsck bool) (JobSnapshot, *JobSnapshot, error) {
	return m.startJob(ctx, JobKindVerify, trigger, repos, fsck)
}

func (m *JobManager) startJob(ctx context.Context, kind JobKind, trigger string, repos []string, fsck bool) (JobSnapshot, *JobSnapshot, error) {
	m.mu.Lock()
	if m.activeID != "" {
		active := m.cloneJobLocked(m.activeID)
		m.mu.Unlock()
		return JobSnapshot{}, active, ErrJobBusy
	}

	randSuffix, _ := newRandomID(4)
	now := time.Now().UTC()
	id := now.Format("20060102-150405") + "-" + randSuffix
	job := &JobSnapshot{
		ID:        id,
		Kind:      kind,
		Trigger:   trigger,
		Repos:     append([]string(nil), repos...),
		Fsck:      fsck,
		Status:    JobRunning,
		StartedAt: now,
	}
	m.activeID = id
	m.jobs[id] = job
	m.order = append([]string{id}, m.order...)
	if len(m.order) > maxJobHistory {
		for _, oldID := range m.order[maxJobHistory:] {
			delete(m.jobs, oldID)
		}
		m.order = m.order[:maxJobHistory]
	}
	snapshot := *m.cloneJobLocked(id)
	m.wg.Add(1)
	m.mu.Unlock()

	go m.runJob(ctx, id, kind, repos, fsck)
	return snapshot, nil, nil
}

func (m *JobManager) runJob(ctx context.Context, id string, kind JobKind, repos []string, fsck bool) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		if m.activeID == id {
			m.activeID = ""
		}
		m.mu.Unlock()
	}()

	onProgress := func(completed, total int, currentRepo string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if j, ok := m.jobs[id]; ok {
			j.Completed = completed
			j.Total = total
			j.CurrentRepo = currentRepo
		}
	}

	switch kind {
	case JobKindSync:
		res := app.RunSyncWithOptions(ctx, m.cfg, m.logger, app.SyncOptions{
			RepoFilter: repos,
			ProgressFn: onProgress,
		}, m.hooks)
		finished := time.Now().UTC()
		m.mu.Lock()
		if j, ok := m.jobs[id]; ok {
			j.FinishedAt = &finished
			code := res.ExitCode
			j.ExitCode = &code
			j.Summary = res.Summary
			j.Error = res.FatalError
			j.CurrentRepo = ""
			switch res.ExitCode {
			case app.ExitOK:
				j.Status = JobSucceeded
			case app.ExitPartial:
				j.Status = JobPartialFailure
			default:
				j.Status = JobFailed
			}
		}
		m.mu.Unlock()

	case JobKindVerify:
		results, code := app.RunVerifyWithResults(ctx, m.cfg, app.VerifyOptions{
			RepoFilters: repos,
			RunFsck:     fsck,
			ProgressFn:  onProgress,
		}, nil, m.hooks)
		finished := time.Now().UTC()
		m.mu.Lock()
		if j, ok := m.jobs[id]; ok {
			j.FinishedAt = &finished
			c := code
			j.ExitCode = &c
			j.VerifyResults = results
			j.CurrentRepo = ""
			switch code {
			case app.ExitOK:
				j.Status = JobSucceeded
			case app.ExitPartial:
				j.Status = JobPartialFailure
			default:
				j.Status = JobFailed
				j.Error = "verify failed due to fatal error or missing matching repository"
			}
		}
		m.mu.Unlock()
	}
}

// List returns recent jobs in reverse chronological order.
func (m *JobManager) List() []JobSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JobSnapshot, 0, len(m.order))
	for _, id := range m.order {
		if j := m.cloneJobLocked(id); j != nil {
			out = append(out, *j)
		}
	}
	return out
}

// Get returns a single job by ID.
func (m *JobManager) Get(id string) (JobSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.cloneJobLocked(id)
	if j == nil {
		return JobSnapshot{}, false
	}
	return *j, true
}

// Active returns the currently running job, if any.
func (m *JobManager) Active() *JobSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeID == "" {
		return nil
	}
	return m.cloneJobLocked(m.activeID)
}

// Wait blocks until all running background jobs complete.
func (m *JobManager) Wait() {
	m.wg.Wait()
}

func (m *JobManager) cloneJobLocked(id string) *JobSnapshot {
	j, ok := m.jobs[id]
	if !ok {
		return nil
	}
	cp := *j
	if j.Repos != nil {
		cp.Repos = append([]string(nil), j.Repos...)
	}
	if j.VerifyResults != nil {
		cp.VerifyResults = append([]gitops.VerifyResult(nil), j.VerifyResults...)
	}
	return &cp
}
