package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

// TargetsFile persists the dynamic repository whitelist in <root_dir>/targets.json.
type TargetsFile struct {
	SchemaVersion int                   `json:"schema_version"`
	UpdatedAt     time.Time             `json:"updated_at"`
	Targets       []config.TargetConfig `json:"targets"`
}

func TargetsPath(rootDir string) string {
	return filepath.Join(rootDir, "targets.json")
}

// LoadOrMigrateTargets loads <root_dir>/targets.json, seeding from seedTargets on first run if absent.
func LoadOrMigrateTargets(rootDir string, seedTargets []config.TargetConfig) (*TargetsFile, bool, error) {
	path := TargetsPath(rootDir)
	raw, err := os.ReadFile(path)
	if err == nil {
		var tf TargetsFile
		if uErr := json.Unmarshal(raw, &tf); uErr != nil {
			return nil, false, fmt.Errorf("parse targets %q: %w", path, uErr)
		}
		if tf.SchemaVersion == 0 {
			tf.SchemaVersion = SchemaVersion
		}
		if tf.Targets == nil {
			tf.Targets = []config.TargetConfig{}
		}
		return &tf, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("read targets %q: %w", path, err)
	}

	tf := &TargetsFile{
		SchemaVersion: SchemaVersion,
		UpdatedAt:     time.Now().UTC(),
		Targets:       make([]config.TargetConfig, 0, len(seedTargets)),
	}
	tf.Targets = append(tf.Targets, seedTargets...)
	if len(tf.Targets) > 0 {
		if err := SaveTargets(rootDir, tf); err != nil {
			return nil, false, fmt.Errorf("seed targets.json: %w", err)
		}
		return tf, true, nil
	}
	return tf, false, nil
}

// SaveTargets atomically persists targets.json to disk.
func SaveTargets(rootDir string, tf *TargetsFile) error {
	tf.SchemaVersion = SchemaVersion
	if tf.Targets == nil {
		tf.Targets = []config.TargetConfig{}
	}
	tf.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(TargetsPath(rootDir), data, 0o644)
}

// Find looks up a target case-insensitively and returns its index and copy.
func (tf *TargetsFile) Find(repo string) (int, *config.TargetConfig) {
	key := strings.ToLower(strings.TrimSpace(repo))
	for i := range tf.Targets {
		if strings.ToLower(tf.Targets[i].Repo) == key {
			cp := tf.Targets[i]
			return i, &cp
		}
	}
	return -1, nil
}

// Add inserts a new repository into the whitelist, returning an error if already present.
func (tf *TargetsFile) Add(tc config.TargetConfig) error {
	norm, err := config.NormalizeRepoName(tc.Repo)
	if err != nil {
		return err
	}
	tc.Repo = norm
	if idx, _ := tf.Find(norm); idx >= 0 {
		return fmt.Errorf("repository %q already exists in targets", norm)
	}
	if tc.CloneTimeout.Duration < 0 || tc.UpdateTimeout.Duration < 0 {
		return errors.New("timeout must not be negative")
	}
	tf.Targets = append(tf.Targets, tc)
	return nil
}

// TargetPatch updates optional per-repository overrides. Nil fields leave existing values unchanged.
type TargetPatch struct {
	EnableLFS          *config.LFSMode  `json:"enable_lfs,omitempty"`
	ClearEnableLFS     bool             `json:"clear_enable_lfs,omitempty"`
	MirrorPRRefs       *bool            `json:"mirror_pr_refs,omitempty"`
	ClearMirrorPRRefs  bool             `json:"clear_mirror_pr_refs,omitempty"`
	SkipUnchanged      *bool            `json:"skip_unchanged,omitempty"`
	ClearSkipUnchanged bool             `json:"clear_skip_unchanged,omitempty"`
	CloneTimeout       *config.Duration `json:"clone_timeout,omitempty"`
	UpdateTimeout      *config.Duration `json:"update_timeout,omitempty"`
}

// Update modifies per-repository overrides for an existing whitelisted repository.
func (tf *TargetsFile) Update(repo string, patch TargetPatch) (config.TargetConfig, error) {
	norm, err := config.NormalizeRepoName(repo)
	if err != nil {
		return config.TargetConfig{}, err
	}
	idx, _ := tf.Find(norm)
	if idx < 0 {
		return config.TargetConfig{}, fmt.Errorf("repository %q not found in targets", norm)
	}
	cur := tf.Targets[idx]
	if patch.ClearEnableLFS {
		cur.EnableLFS = nil
	} else if patch.EnableLFS != nil {
		val := *patch.EnableLFS
		cur.EnableLFS = &val
	}
	if patch.ClearMirrorPRRefs {
		cur.MirrorPRRefs = nil
	} else if patch.MirrorPRRefs != nil {
		val := *patch.MirrorPRRefs
		cur.MirrorPRRefs = &val
	}
	if patch.ClearSkipUnchanged {
		cur.SkipUnchanged = nil
	} else if patch.SkipUnchanged != nil {
		val := *patch.SkipUnchanged
		cur.SkipUnchanged = &val
	}
	if patch.CloneTimeout != nil {
		if patch.CloneTimeout.Duration < 0 {
			return config.TargetConfig{}, errors.New("clone_timeout must not be negative")
		}
		cur.CloneTimeout = *patch.CloneTimeout
	}
	if patch.UpdateTimeout != nil {
		if patch.UpdateTimeout.Duration < 0 {
			return config.TargetConfig{}, errors.New("update_timeout must not be negative")
		}
		cur.UpdateTimeout = *patch.UpdateTimeout
	}
	tf.Targets[idx] = cur
	return cur, nil
}

// Remove deletes a repository from the whitelist.
func (tf *TargetsFile) Remove(repo string) (string, bool, error) {
	norm, err := config.NormalizeRepoName(repo)
	if err != nil {
		return "", false, err
	}
	idx, _ := tf.Find(norm)
	if idx < 0 {
		return norm, false, nil
	}
	removedName := tf.Targets[idx].Repo
	tf.Targets = append(tf.Targets[:idx], tf.Targets[idx+1:]...)
	return removedName, true, nil
}
