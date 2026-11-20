package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const SchemaVersion = 2

type RepoStatus string

const (
	StatusCloned    RepoStatus = "cloned"
	StatusUpdated   RepoStatus = "updated"
	StatusUpToDate  RepoStatus = "up_to_date"
	StatusFailed    RepoStatus = "failed"
	StatusMissing   RepoStatus = "missing"
	StatusUnmanaged RepoStatus = "unmanaged"
)

var ErrLockBusy = errors.New("backup lock is currently held by another process")

// RepoLedger stores per-repository sync state keyed by immutable GitHub repo ID.
type RepoLedger struct {
	SchemaVersion int                    `json:"schema_version"`
	UpdatedAt     time.Time              `json:"updated_at"`
	Repositories  map[string]*RepoRecord `json:"repositories"`
}

type RepoRecord struct {
	ID                  int64      `json:"id"`
	FullName            string     `json:"full_name"`
	LocalPath           string     `json:"local_path"`
	CloneURL            string     `json:"clone_url"`
	State               RepoStatus `json:"state"`
	ETag                string     `json:"etag"`
	RemotePushedAt      *time.Time `json:"remote_pushed_at,omitempty"`
	LastGitSyncAt       *time.Time `json:"last_git_sync_at,omitempty"`
	LastCheckedAt       *time.Time `json:"last_checked_at,omitempty"`
	LastAlertAt         *time.Time `json:"last_alert_at,omitempty"`
	HeadCommit          string     `json:"head_commit"`
	DiskSizeBytes       int64      `json:"disk_size_bytes"`
	LFSEnabled          bool       `json:"lfs_enabled"`
	SyncCountSinceGC    int        `json:"sync_count_since_gc"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error"`
}

// StatusSummary captures the latest sync run metrics and abnormal repositories.
type StatusSummary struct {
	SchemaVersion             int              `json:"schema_version"`
	RunID                     string           `json:"run_id"`
	StartedAt                 time.Time        `json:"started_at"`
	FinishedAt                time.Time        `json:"finished_at"`
	DurationMS                int64            `json:"duration_ms"`
	ExitCode                  int              `json:"exit_code"`
	BulkMissingAlertTriggered bool             `json:"bulk_missing_alert_triggered"`
	Counts                    SummaryCounts    `json:"counts"`
	AbnormalRepos             []AbnormalReport `json:"abnormal_repos"`
}

type SummaryCounts struct {
	ManagedTotal int `json:"managed_total"`
	Cloned       int `json:"cloned"`
	Updated      int `json:"updated"`
	UpToDate     int `json:"up_to_date"`
	Failed       int `json:"failed"`
	Missing      int `json:"missing"`
	Unmanaged    int `json:"unmanaged"`
}

type AbnormalReport struct {
	Repo                string     `json:"repo"`
	State               RepoStatus `json:"state"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error"`
}

// FileLock wraps an exclusive non-blocking flock handle on .backup.lock.
type FileLock struct {
	file *os.File
}

// AcquireLock opens rootDir/.backup.lock and acquires a non-blocking exclusive flock.
func AcquireLock(rootDir string) (*FileLock, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure root_dir %q: %w", rootDir, err)
	}
	lockPath := filepath.Join(rootDir, ".backup.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLockBusy
		}
		return nil, fmt.Errorf("acquire flock on %q: %w", lockPath, err)
	}
	return &FileLock{file: f}, nil
}

func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func NewEmptyLedger() *RepoLedger {
	return &RepoLedger{
		SchemaVersion: SchemaVersion,
		Repositories:  make(map[string]*RepoRecord),
	}
}

func LedgerPath(rootDir string) string {
	return filepath.Join(rootDir, "repo_ledger.json")
}

func SummaryPath(rootDir string) string {
	return filepath.Join(rootDir, "status_summary.json")
}

func LoadLedger(rootDir string) (*RepoLedger, error) {
	path := LedgerPath(rootDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewEmptyLedger(), nil
		}
		return nil, fmt.Errorf("read ledger %q: %w", path, err)
	}
	var l RepoLedger
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("parse ledger %q: %w", path, err)
	}
	if l.Repositories == nil {
		l.Repositories = make(map[string]*RepoRecord)
	}
	if l.SchemaVersion == 0 {
		l.SchemaVersion = SchemaVersion
	}
	return &l, nil
}

func SaveLedger(rootDir string, l *RepoLedger) error {
	l.SchemaVersion = SchemaVersion
	if l.Repositories == nil {
		l.Repositories = make(map[string]*RepoRecord)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(LedgerPath(rootDir), data, 0o644)
}

func LoadSummary(rootDir string) (*StatusSummary, error) {
	path := SummaryPath(rootDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s StatusSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse summary %q: %w", path, err)
	}
	return &s, nil
}

func SaveSummary(rootDir string, s *StatusSummary) error {
	s.SchemaVersion = SchemaVersion
	if s.AbnormalRepos == nil {
		s.AbnormalRepos = []AbnormalReport{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(SummaryPath(rootDir), data, 0o644)
}

// WriteFileAtomic writes data to a sibling temp file, fsyncs it, and atomically renames over targetPath.
func WriteFileAtomic(targetPath string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// FindByFullName looks up a repository record case-insensitively by full_name.
func (l *RepoLedger) FindByFullName(fullName string) *RepoRecord {
	target := strings.ToLower(strings.TrimSpace(fullName))
	for _, rec := range l.Repositories {
		if strings.ToLower(rec.FullName) == target {
			return rec
		}
	}
	return nil
}

// FindByLocalPath returns any ledger record occupying the given relative local path.
func (l *RepoLedger) FindByLocalPath(relPath string) *RepoRecord {
	clean := filepath.Clean(relPath)
	for _, rec := range l.Repositories {
		if filepath.Clean(rec.LocalPath) == clean {
			return rec
		}
	}
	return nil
}

func RepoIDKey(id int64) string {
	return strconv.FormatInt(id, 10)
}

func RelativeRepoPath(fullName string) string {
	return filepath.ToSlash(filepath.Join("repos", fullName+".git"))
}

// DiskInfo captures filesystem capacity metrics for storage.root_dir.
type DiskInfo struct {
	TotalBytes   uint64  `json:"total_bytes"`
	AvailBytes   uint64  `json:"avail_bytes"`
	UsedBytes    uint64  `json:"used_bytes"`
	AvailPercent float64 `json:"avail_percent"`
}

// GetDiskInfo inspects the filesystem hosting rootDir.
func GetDiskInfo(rootDir string) (DiskInfo, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return DiskInfo{}, fmt.Errorf("ensure root_dir %q: %w", rootDir, err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(rootDir, &st); err != nil {
		return DiskInfo{}, fmt.Errorf("statfs %q: %w", rootDir, err)
	}
	if st.Blocks == 0 || st.Bsize <= 0 {
		return DiskInfo{}, nil
	}
	totalBytes := st.Blocks * uint64(st.Bsize)
	availBytes := st.Bavail * uint64(st.Bsize)
	freeBytes := st.Bfree * uint64(st.Bsize)
	var usedBytes uint64
	if totalBytes >= freeBytes {
		usedBytes = totalBytes - freeBytes
	}
	availPercent := (float64(availBytes) / float64(totalBytes)) * 100.0
	return DiskInfo{
		TotalBytes:   totalBytes,
		AvailBytes:   availBytes,
		UsedBytes:    usedBytes,
		AvailPercent: availPercent,
	}, nil
}

// CheckDiskSpace validates free disk thresholds using both percentage and absolute GB bounds.
func CheckDiskSpace(rootDir string, minPercent, minGB float64) error {
	info, err := GetDiskInfo(rootDir)
	if err != nil {
		return err
	}
	if info.TotalBytes == 0 {
		return nil
	}
	availGB := float64(info.AvailBytes) / (1024 * 1024 * 1024)

	if minPercent > 0 && info.AvailPercent < minPercent {
		return fmt.Errorf("insufficient disk space on %s: %.2f%% free (< %.2f%% threshold)", rootDir, info.AvailPercent, minPercent)
	}
	if minGB > 0 && availGB < minGB {
		return fmt.Errorf("insufficient disk space on %s: %.2f GB free (< %.2f GB threshold)", rootDir, availGB, minGB)
	}
	return nil
}

// DirSizeBytes computes total bytes occupied by files under path.
func DirSizeBytes(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// CleanTmpClones clears leftover staging directories from previous interrupted clones.
func CleanTmpClones(rootDir string) error {
	tmpRoot := filepath.Join(rootDir, ".tmp_clones")
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(tmpRoot, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// CleanupTrash removes .trash entries older than retentionDays and prunes archived ledger records.
func CleanupTrash(rootDir string, retentionDays int, l *RepoLedger, now time.Time) (int, error) {
	trashRoot := filepath.Join(rootDir, ".trash")
	entries, err := os.ReadDir(trashRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fullPath := filepath.Join(trashRoot, e.Name())
		ts, ok := parseTrashTimestamp(e.Name())
		if !ok {
			info, infoErr := e.Info()
			if infoErr != nil {
				continue
			}
			ts = info.ModTime()
		}
		if ts.Before(cutoff) {
			if err := os.RemoveAll(fullPath); err != nil {
				return removed, err
			}
			removed++
		}
	}

	// Reclaim ledger entries whose archived local_path under .trash no longer exists on disk.
	if l != nil {
		for id, rec := range l.Repositories {
			if strings.HasPrefix(filepath.ToSlash(rec.LocalPath), ".trash/") {
				if _, statErr := os.Stat(filepath.Join(rootDir, rec.LocalPath)); errors.Is(statErr, os.ErrNotExist) {
					delete(l.Repositories, id)
				}
			}
		}
	}
	return removed, nil
}

func parseTrashTimestamp(name string) (time.Time, bool) {
	idx := strings.IndexByte(name, '_')
	if idx < 0 || idx+1 >= len(name) {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102_150405", name[idx+1:])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
