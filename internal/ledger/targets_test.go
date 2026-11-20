package ledger

import (
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/config"
)

func TestTargetsStoreMigrateAddPatchRemove(t *testing.T) {
	root := t.TempDir()
	seed := []config.TargetConfig{{Repo: "muidea/magicCommon"}}

	tf, migrated, err := LoadOrMigrateTargets(root, seed)
	if err != nil || !migrated || len(tf.Targets) != 1 {
		t.Fatalf("unexpected migration result: migrated=%v len=%d err=%v", migrated, len(tf.Targets), err)
	}

	// Second load reads existing targets.json without migrating.
	tf2, migrated2, err := LoadOrMigrateTargets(root, nil)
	if err != nil || migrated2 || len(tf2.Targets) != 1 {
		t.Fatalf("unexpected second load: migrated=%v len=%d err=%v", migrated2, len(tf2.Targets), err)
	}

	// Add via SSH URL normalization.
	if err := tf2.Add(config.TargetConfig{Repo: "git@github.com:muidea/magicRunner.git"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := tf2.Add(config.TargetConfig{Repo: "muidea/magicrunner"}); err == nil {
		t.Fatal("expected duplicate error on case-insensitive add")
	}

	lfs := config.LFSTrue
	updated, err := tf2.Update("muidea/magicRunner", TargetPatch{
		EnableLFS:     &lfs,
		UpdateTimeout: &config.Duration{Duration: 30 * time.Minute},
	})
	if err != nil || updated.EnableLFS == nil || *updated.EnableLFS != config.LFSTrue {
		t.Fatalf("Update failed: updated=%+v err=%v", updated, err)
	}

	if err := SaveTargets(root, tf2); err != nil {
		t.Fatalf("SaveTargets failed: %v", err)
	}

	tf3, _, err := LoadOrMigrateTargets(root, nil)
	if err != nil || len(tf3.Targets) != 2 {
		t.Fatalf("reload failed: len=%d err=%v", len(tf3.Targets), err)
	}

	name, removed, err := tf3.Remove("muidea/magicCommon")
	if err != nil || !removed || name != "muidea/magicCommon" || len(tf3.Targets) != 1 {
		t.Fatalf("Remove failed: name=%q removed=%v len=%d err=%v", name, removed, len(tf3.Targets), err)
	}
}
