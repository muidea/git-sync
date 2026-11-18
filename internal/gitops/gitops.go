package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

const (
	staleLockAge = time.Hour
	fsckTimeout  = 5 * time.Minute
)

var gitVersionRegex = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// RefMismatch describes a single reference discrepancy during verify.
type RefMismatch struct {
	Ref       string `json:"ref"`
	RemoteSHA string `json:"remote_sha,omitempty"`
	LocalSHA  string `json:"local_sha,omitempty"`
	Kind      string `json:"kind"` // missing_local | extra_local | sha_mismatch
}

// VerifyResult holds the reference comparison and optional fsck result for one repository.
type VerifyResult struct {
	Repo       string        `json:"repo"`
	OK         bool          `json:"ok"`
	Mismatches []RefMismatch `json:"mismatches,omitempty"`
	FsckOK     *bool         `json:"fsck_ok,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// CheckGitVersion ensures the installed git binary is >= 2.31 (required for GIT_CONFIG_COUNT).
func CheckGitVersion(ctx context.Context) error {
	out, err := runCmd(ctx, "", nil, "git", "--version")
	if err != nil {
		return fmt.Errorf("git executable check failed: %w", err)
	}
	m := gitVersionRegex.FindStringSubmatch(out)
	if len(m) < 3 {
		return fmt.Errorf("unable to parse git version from %q", strings.TrimSpace(out))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || (major == 2 && minor < 31) {
		return fmt.Errorf("git >= 2.31 required for GIT_CONFIG_COUNT env auth injection (found %d.%d)", major, minor)
	}
	return nil
}

// HasGitLFS returns true when `git lfs version` succeeds on the host.
func HasGitLFS(ctx context.Context) bool {
	_, err := runCmd(ctx, "", nil, "git", "lfs", "version")
	return err == nil
}

// CloneMirrorAtomic clones into tmpDir and atomically moves the completed bare mirror to destDir.
func CloneMirrorAtomic(
	ctx context.Context,
	tmpDir string,
	destDir string,
	cloneURL string,
	mirrorPRRefs bool,
	env []string,
) error {
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(filepath.Dir(tmpDir), 0o755); err != nil {
		return err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if mirrorPRRefs {
		if _, err := runCmd(ctx, "", env, "git", "clone", "--mirror", cloneURL, tmpDir); err != nil {
			return fmt.Errorf("git clone --mirror: %w", err)
		}
	} else {
		if _, err := runCmd(ctx, "", env, "git", "clone", "--bare", cloneURL, tmpDir); err != nil {
			return fmt.Errorf("git clone --bare: %w", err)
		}
		if err := setRefspecs(ctx, tmpDir, false); err != nil {
			return err
		}
		if _, err := runCmd(ctx, tmpDir, env, "git", "remote", "update", "--prune"); err != nil {
			return fmt.Errorf("initial refspec update: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, destDir); err != nil {
		return fmt.Errorf("atomic move clone to %q: %w", destDir, err)
	}
	cleanupTmp = false
	return nil
}

// EnsureRefspec aligns remote.origin.fetch in an existing mirror repository with mirrorPRRefs.
func EnsureRefspec(ctx context.Context, repoDir string, mirrorPRRefs bool) error {
	out, err := runCmd(ctx, repoDir, nil, "git", "config", "--get-all", "remote.origin.fetch")
	if err != nil && !isExitCode(err, 1) {
		return fmt.Errorf("read remote.origin.fetch: %w", err)
	}
	desired := desiredRefspecs(mirrorPRRefs)
	current := nonEmptyLines(out)
	if stringSlicesEqual(current, desired) {
		return nil
	}
	return setRefspecs(ctx, repoDir, mirrorPRRefs)
}

// UpdateMirror performs `git remote update --prune` with D/F conflict and stale .lock self-healing.
func UpdateMirror(ctx context.Context, repoDir string, env []string) error {
	_, err := runCmd(ctx, repoDir, env, "git", "remote", "update", "--prune")
	if err == nil {
		return nil
	}

	errMsg := err.Error()
	if isDFConflictError(errMsg) {
		if _, pruneErr := runCmd(ctx, repoDir, env, "git", "remote", "prune", "origin"); pruneErr == nil {
			if _, retryErr := runCmd(ctx, repoDir, env, "git", "remote", "update", "--prune"); retryErr == nil {
				return nil
			} else {
				err = retryErr
			}
		}
	} else if isLockFileError(errMsg) {
		if removed, healErr := removeStaleLocksAndCheck(ctx, repoDir); healErr == nil && removed > 0 {
			if _, retryErr := runCmd(ctx, repoDir, env, "git", "remote", "update", "--prune"); retryErr == nil {
				return nil
			} else {
				err = retryErr
			}
		} else if healErr != nil {
			err = fmt.Errorf("%w (self-heal failed: %v)", err, healErr)
		}
	}

	cleanupTmpPacks(repoDir)
	return err
}

// SetRemoteURL updates origin's URL after repository rename.
func SetRemoteURL(ctx context.Context, repoDir, newURL string) error {
	_, err := runCmd(ctx, repoDir, nil, "git", "remote", "set-url", "origin", newURL)
	return err
}

// IsBareMirrorFor validates an existing directory for cold-start adoption.
func IsBareMirrorFor(ctx context.Context, repoDir, fullName string) bool {
	out, err := runCmd(ctx, repoDir, nil, "git", "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(out) != "true" {
		return false
	}
	urlOut, err := runCmd(ctx, repoDir, nil, "git", "config", "--get", "remote.origin.url")
	if err != nil {
		return false
	}
	got := normalizeGitHubURL(urlOut)
	expected := strings.ToLower("https://github.com/" + strings.TrimSuffix(fullName, ".git"))
	return got == expected
}

// HeadCommit returns "EMPTY" for empty repos or the resolved HEAD commit SHA.
func HeadCommit(ctx context.Context, repoDir string) (string, error) {
	refsOut, err := runCmd(ctx, repoDir, nil, "git", "show-ref")
	if err != nil && !isExitCode(err, 1) {
		return "", fmt.Errorf("git show-ref: %w", err)
	}
	lines := nonEmptyLines(refsOut)
	if len(lines) == 0 {
		return "EMPTY", nil
	}

	headOut, err := runCmd(ctx, repoDir, nil, "git", "rev-parse", "HEAD")
	if err == nil {
		return strings.TrimSpace(headOut), nil
	}
	// Fallback to first branch ref if HEAD points to a renamed/absent default branch.
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "refs/heads/") {
			return fields[0], nil
		}
	}
	fields := strings.Fields(lines[0])
	if len(fields) >= 1 {
		return fields[0], nil
	}
	return "EMPTY", nil
}

// ShouldFetchLFS determines whether LFS objects should be fetched for repoDir.
func ShouldFetchLFS(ctx context.Context, repoDir string, mode config.LFSMode) (bool, error) {
	switch mode {
	case config.LFSTrue:
		return true, nil
	case config.LFSFalse:
		return false, nil
	case config.LFSAuto:
		if info, err := os.Stat(filepath.Join(repoDir, "lfs")); err == nil && info.IsDir() {
			return true, nil
		}
		if attrs, err := runCmd(ctx, repoDir, nil, "git", "show", "HEAD:.gitattributes"); err == nil {
			if strings.Contains(attrs, "filter=lfs") {
				return true, nil
			}
		}
		if HasGitLFS(ctx) {
			if out, err := runCmd(ctx, repoDir, nil, "git", "lfs", "ls-files", "--all"); err == nil {
				if len(strings.TrimSpace(out)) > 0 {
					return true, nil
				}
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unknown lfs mode %q", mode)
	}
}

// FetchLFS runs `git lfs fetch --all`, failing explicitly if git-lfs is missing.
func FetchLFS(ctx context.Context, repoDir string, env []string) error {
	if !HasGitLFS(ctx) {
		return errors.New("repository requires Git LFS, but git-lfs binary is not installed on host")
	}
	_, err := runCmd(ctx, repoDir, env, "git", "lfs", "fetch", "--all")
	return err
}

// RunMaintenance runs `git maintenance run --auto` on repoDir.
func RunMaintenance(ctx context.Context, repoDir string) error {
	_, err := runCmd(ctx, repoDir, nil, "git", "maintenance", "run", "--auto")
	return err
}

// VerifyRefs compares remote `git ls-remote` against local `git show-ref` after normalizing special refs.
func VerifyRefs(
	ctx context.Context,
	repoDir string,
	repoName string,
	remoteURL string,
	mirrorPRRefs bool,
	runFsck bool,
	env []string,
) VerifyResult {
	res := VerifyResult{Repo: repoName}

	remoteOut, err := runCmd(ctx, "", env, "git", "ls-remote", remoteURL)
	if err != nil {
		res.Error = fmt.Sprintf("git ls-remote failed: %v", err)
		return res
	}
	localOut, err := runCmd(ctx, repoDir, nil, "git", "show-ref")
	if err != nil && !isExitCode(err, 1) {
		res.Error = fmt.Sprintf("git show-ref failed: %v", err)
		return res
	}

	remoteRefs := parseNormalizedRefs(remoteOut, true, mirrorPRRefs)
	localRefs := parseNormalizedRefs(localOut, false, mirrorPRRefs)

	for ref, rSHA := range remoteRefs {
		lSHA, ok := localRefs[ref]
		if !ok {
			res.Mismatches = append(res.Mismatches, RefMismatch{
				Ref:       ref,
				RemoteSHA: rSHA,
				Kind:      "missing_local",
			})
			continue
		}
		if lSHA != rSHA {
			res.Mismatches = append(res.Mismatches, RefMismatch{
				Ref:       ref,
				RemoteSHA: rSHA,
				LocalSHA:  lSHA,
				Kind:      "sha_mismatch",
			})
		}
	}
	for ref, lSHA := range localRefs {
		if _, ok := remoteRefs[ref]; !ok {
			res.Mismatches = append(res.Mismatches, RefMismatch{
				Ref:      ref,
				LocalSHA: lSHA,
				Kind:     "extra_local",
			})
		}
	}

	fsckPassed := true
	if runFsck {
		fsckCtx, cancel := context.WithTimeout(ctx, fsckTimeout)
		defer cancel()
		if _, fsckErr := runCmd(fsckCtx, repoDir, nil, "git", "fsck", "--no-dangling"); fsckErr != nil {
			fsckPassed = false
			res.Error = fmt.Sprintf("git fsck failed: %v", fsckErr)
		}
		res.FsckOK = &fsckPassed
	}

	res.OK = len(res.Mismatches) == 0 && fsckPassed && res.Error == ""
	return res
}

func parseNormalizedRefs(output string, isRemote bool, mirrorPRRefs bool) map[string]string {
	refs := make(map[string]string)
	for _, line := range nonEmptyLines(output) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		if ref == "HEAD" || strings.HasSuffix(ref, "^{}") {
			continue
		}
		if isRemote && strings.HasPrefix(ref, "refs/pull/") {
			if !mirrorPRRefs || strings.HasSuffix(ref, "/merge") {
				continue
			}
		}
		if !isRemote && strings.HasPrefix(ref, "refs/pull/") {
			if !mirrorPRRefs || strings.HasSuffix(ref, "/merge") {
				continue
			}
		}
		refs[ref] = sha
	}
	return refs
}

func setRefspecs(ctx context.Context, repoDir string, mirrorPRRefs bool) error {
	if _, err := runCmd(ctx, repoDir, nil, "git", "config", "--unset-all", "remote.origin.fetch"); err != nil && !isExitCode(err, 5) {
		return fmt.Errorf("unset remote.origin.fetch: %w", err)
	}
	for _, spec := range desiredRefspecs(mirrorPRRefs) {
		if _, err := runCmd(ctx, repoDir, nil, "git", "config", "--add", "remote.origin.fetch", spec); err != nil {
			return fmt.Errorf("add remote.origin.fetch %q: %w", spec, err)
		}
	}
	return nil
}

func desiredRefspecs(mirrorPRRefs bool) []string {
	if mirrorPRRefs {
		return []string{"+refs/*:refs/*"}
	}
	return []string{
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
		"+refs/notes/*:refs/notes/*",
	}
}

func removeStaleLocksAndCheck(ctx context.Context, repoDir string) (int, error) {
	cutoff := time.Now().Add(-staleLockAge)
	removed := 0
	err := filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return rmErr
			}
			removed++
		}
		return nil
	})
	if err != nil || removed == 0 {
		return removed, err
	}
	fsckCtx, cancel := context.WithTimeout(ctx, fsckTimeout)
	defer cancel()
	if _, fsckErr := runCmd(fsckCtx, repoDir, nil, "git", "fsck", "--no-dangling"); fsckErr != nil {
		return removed, fmt.Errorf("git fsck after lock cleanup: %w", fsckErr)
	}
	return removed, nil
}

func cleanupTmpPacks(repoDir string) {
	packDir := filepath.Join(repoDir, "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp_") {
			_ = os.RemoveAll(filepath.Join(packDir, e.Name()))
		}
	}
}

func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = strings.TrimSpace(stdout.String())
		}
		if errText != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, errText)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func isDFConflictError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "cannot lock ref") &&
		(strings.Contains(low, "exists") || strings.Contains(low, "cannot create"))
}

func isLockFileError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, ".lock") && strings.Contains(low, "file exists")
}

func normalizeGitHubURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")
	if strings.HasPrefix(s, "git@github.com:") {
		s = "https://github.com/" + strings.TrimPrefix(s, "git@github.com:")
	}
	return strings.ToLower(s)
}

func nonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	var out []string
	for _, line := range raw {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
