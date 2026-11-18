package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockAcquireAndContention(t *testing.T) {
	root := t.TempDir()
	lock1, err := AcquireLock(root)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}
	defer lock1.Release()

	_, err = AcquireLock(root)
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("expected ErrLockBusy, got %v", err)
	}

	if err := lock1.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	lock2, err := AcquireLock(root)
	if err != nil {
		t.Fatalf("re-acquire after release failed: %v", err)
	}
	_ = lock2.Release()
}

func TestLedgerSaveLoadAndTrashCleanup(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	oldArchive := filepath.Join(root, ".trash", "unmanaged_20261001_000000", "muidea", "old.git")
	newArchive := filepath.Join(root, ".trash", "conflict_20261115_000000", "muidea", "keep.git")
	if err := os.MkdirAll(oldArchive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newArchive, 0o755); err != nil {
		t.Fatal(err)
	}

	l := NewEmptyLedger()
	l.Repositories["100"] = &RepoRecord{
		ID:        100,
		FullName:  "muidea/old",
		LocalPath: ".trash/unmanaged_20261001_000000/muidea/old.git",
		State:     StatusUnmanaged,
	}
	l.Repositories["200"] = &RepoRecord{
		ID:        200,
		FullName:  "muidea/keep",
		LocalPath: ".trash/conflict_20261115_000000/muidea/keep.git",
		State:     StatusUnmanaged,
	}

	removed, err := CleanupTrash(root, 30, l, now)
	if err != nil {
		t.Fatalf("CleanupTrash failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 expired trash dir removed, got %d", removed)
	}
	if _, ok := l.Repositories["100"]; ok {
		t.Fatalf("expected expired archived repo 100 to be purged from ledger")
	}
	if _, ok := l.Repositories["200"]; !ok {
		t.Fatalf("expected non-expired archived repo 200 to remain in ledger")
	}

	if err := SaveLedger(root, l); err != nil {
		t.Fatalf("SaveLedger failed: %v", err)
	}
	loaded, err := LoadLedger(root)
	if err != nil {
		t.Fatalf("LoadLedger failed: %v", err)
	}
	if len(loaded.Repositories) != 1 || loaded.Repositories["200"].FullName != "muidea/keep" {
		t.Fatalf("unexpected loaded ledger: %+v", loaded)
	}
}
