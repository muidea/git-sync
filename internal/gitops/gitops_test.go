package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return string(out)
}

func setupRemoteWithCommitAndTag(t *testing.T) (remoteBare string, workDir string) {
	t.Helper()
	root := t.TempDir()
	remoteBare = filepath.Join(root, "remote.git")
	workDir = filepath.Join(root, "work")

	runGit(t, "", "init", "--bare", remoteBare)
	runGit(t, "", "clone", remoteBare, workDir)
	runGit(t, workDir, "config", "user.email", "test@example.com")
	runGit(t, workDir, "config", "user.name", "Tester")

	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", "README.md")
	runGit(t, workDir, "commit", "-m", "init")
	runGit(t, workDir, "branch", "-M", "main")
	runGit(t, workDir, "push", "-u", "origin", "main")
	runGit(t, workDir, "tag", "-a", "v1.0.0", "-m", "release v1.0.0")
	runGit(t, workDir, "push", "origin", "v1.0.0")
	return remoteBare, workDir
}

func TestCloneUpdateForcePushVerifyAndDFConflict(t *testing.T) {
	ctx := context.Background()
	if err := CheckGitVersion(ctx); err != nil {
		t.Fatalf("CheckGitVersion failed: %v", err)
	}

	remoteBare, workDir := setupRemoteWithCommitAndTag(t)
	backupRoot := t.TempDir()
	tmpDir := filepath.Join(backupRoot, ".tmp_clones", "org", "repo.git")
	destDir := filepath.Join(backupRoot, "repos", "org", "repo.git")

	if err := CloneMirrorAtomic(ctx, tmpDir, destDir, remoteBare, true, nil); err != nil {
		t.Fatalf("CloneMirrorAtomic failed: %v", err)
	}

	sha1, err := HeadCommit(ctx, destDir)
	if err != nil || sha1 == "" || sha1 == "EMPTY" {
		t.Fatalf("unexpected head commit: %q err=%v", sha1, err)
	}

	// Verify annotated tag + branch match (must ignore ^{} and HEAD)
	ver := VerifyRefs(ctx, destDir, "org/repo", remoteBare, true, true, nil)
	if !ver.OK {
		t.Fatalf("expected VerifyRefs OK, got %+v", ver)
	}

	// Simulate D/F branch transition: push `feat`, sync, delete `feat`, push `feat/login`, sync
	runGit(t, workDir, "checkout", "-b", "feat")
	_ = os.WriteFile(filepath.Join(workDir, "a.txt"), []byte("1"), 0o644)
	runGit(t, workDir, "add", "a.txt")
	runGit(t, workDir, "commit", "-m", "feat")
	runGit(t, workDir, "push", "origin", "feat")
	if err := UpdateMirror(ctx, destDir, nil); err != nil {
		t.Fatalf("UpdateMirror feat failed: %v", err)
	}

	runGit(t, workDir, "checkout", "main")
	runGit(t, workDir, "branch", "-D", "feat")
	runGit(t, workDir, "push", "origin", "--delete", "feat")
	runGit(t, workDir, "checkout", "-b", "feat/login")
	_ = os.WriteFile(filepath.Join(workDir, "b.txt"), []byte("2"), 0o644)
	runGit(t, workDir, "add", "b.txt")
	runGit(t, workDir, "commit", "-m", "feat/login")
	runGit(t, workDir, "push", "origin", "feat/login")

	if err := UpdateMirror(ctx, destDir, nil); err != nil {
		t.Fatalf("UpdateMirror after D/F branch transition failed: %v", err)
	}

	ver2 := VerifyRefs(ctx, destDir, "org/repo", remoteBare, true, false, nil)
	if !ver2.OK {
		t.Fatalf("expected VerifyRefs OK after D/F transition, got %+v", ver2)
	}

	// Test EnsureRefspec toggle from mirrorPRRefs=true to false
	if err := EnsureRefspec(ctx, destDir, false); err != nil {
		t.Fatalf("EnsureRefspec(false) failed: %v", err)
	}
	if err := EnsureRefspec(ctx, destDir, false); err != nil {
		t.Fatalf("EnsureRefspec(false) idempotent check failed: %v", err)
	}
}

func TestEmptyRepoAndStaleLockCleanup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	emptyRemote := filepath.Join(root, "empty-remote.git")
	runGit(t, "", "init", "--bare", emptyRemote)

	tmpDir := filepath.Join(root, ".tmp", "empty.git")
	destDir := filepath.Join(root, "repos", "empty.git")
	if err := CloneMirrorAtomic(ctx, tmpDir, destDir, emptyRemote, true, nil); err != nil {
		t.Fatalf("CloneMirrorAtomic empty failed: %v", err)
	}
	head, err := HeadCommit(ctx, destDir)
	if err != nil || head != "EMPTY" {
		t.Fatalf("expected EMPTY head, got %q err=%v", head, err)
	}
	lfsNeeded, err := ShouldFetchLFS(ctx, destDir, config.LFSAuto)
	if err != nil || lfsNeeded {
		t.Fatalf("expected no LFS on empty repo, got %v err=%v", lfsNeeded, err)
	}

	// Verify stale lock removal (>1h old)
	staleLock := filepath.Join(destDir, "packed-refs.lock")
	if err := os.WriteFile(staleLock, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleLock, twoHoursAgo, twoHoursAgo); err != nil {
		t.Fatal(err)
	}
	removed, err := removeStaleLocksAndCheck(ctx, destDir)
	if err != nil || removed != 1 {
		t.Fatalf("expected 1 stale lock removed, got removed=%d err=%v", removed, err)
	}
}

func TestCheckBareMirrorForAndURLNormalization(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		raw  string
		want string
	}{
		{"https://github.com/muidea/Repo.git", "https://github.com/muidea/repo"},
		{"https://x-access-token:ghp_secret@github.com/muidea/Repo.git", "https://github.com/muidea/repo"},
		{"http://github.com/muidea/Repo/", "https://github.com/muidea/repo"},
		{"ssh://git@github.com/muidea/Repo.git", "https://github.com/muidea/repo"},
		{"git://github.com/muidea/Repo.git", "https://github.com/muidea/repo"},
		{"git@github.com:muidea/Repo.git", "https://github.com/muidea/repo"},
	}
	for _, tc := range cases {
		if got := normalizeGitHubURL(tc.raw); got != tc.want {
			t.Fatalf("normalizeGitHubURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	root := t.TempDir()
	bareDir := filepath.Join(root, "bare.git")
	runGit(t, "", "init", "--bare", bareDir)
	runGit(t, bareDir, "remote", "add", "origin", "https://x-access-token:ghp_topsecret@github.com/muidea/Adopt.git")

	if ok, reason := CheckBareMirrorFor(ctx, bareDir, "muidea/adopt"); !ok {
		t.Fatalf("expected CheckBareMirrorFor to adopt credentialed origin URL, reason=%s", reason)
	}

	runGit(t, bareDir, "remote", "set-url", "origin", "ssh://git@github.com/muidea/Adopt.git")
	if ok, reason := CheckBareMirrorFor(ctx, bareDir, "muidea/adopt"); !ok {
		t.Fatalf("expected CheckBareMirrorFor to adopt ssh:// origin URL, reason=%s", reason)
	}

	runGit(t, bareDir, "remote", "set-url", "origin", "https://x-access-token:ghp_topsecret@github.com/muidea/Other.git")
	ok, reason := CheckBareMirrorFor(ctx, bareDir, "muidea/adopt")
	if ok || !strings.Contains(reason, "mismatch") || strings.Contains(reason, "ghp_topsecret") {
		t.Fatalf("unexpected mismatch result ok=%v reason=%q", ok, reason)
	}

	nonBare := filepath.Join(root, "nonbare")
	runGit(t, "", "init", nonBare)
	ok, reason = CheckBareMirrorFor(ctx, nonBare, "muidea/adopt")
	if ok || !strings.Contains(reason, "not a bare repository") {
		t.Fatalf("expected non-bare reason, got ok=%v reason=%q", ok, reason)
	}
}
