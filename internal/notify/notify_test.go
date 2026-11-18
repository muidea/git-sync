package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/ledger"
)

func TestWritePrometheus(t *testing.T) {
	dir := t.TempDir()
	promPath := filepath.Join(dir, "git_backup.prom")

	summary := &ledger.StatusSummary{
		FinishedAt: time.Unix(1795000000, 0).UTC(),
		DurationMS: 12340,
		Counts: ledger.SummaryCounts{
			Cloned:    1,
			Updated:   2,
			UpToDate:  5,
			Failed:    1,
			Missing:   1,
			Unmanaged: 3,
		},
	}
	l := ledger.NewEmptyLedger()
	l.Repositories["1"] = &ledger.RepoRecord{DiskSizeBytes: 1000}
	l.Repositories["2"] = &ledger.RepoRecord{DiskSizeBytes: 2500}

	if err := WritePrometheus(promPath, summary, l); err != nil {
		t.Fatalf("WritePrometheus failed: %v", err)
	}
	raw, err := os.ReadFile(promPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"git_backup_last_run_timestamp_seconds 1795000000",
		"git_backup_duration_seconds 12.340",
		`git_backup_repos_total{status="missing"} 1`,
		"git_backup_storage_bytes_total 3500",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing metric line %q in:\n%s", want, content)
		}
	}
}

func TestWebhookFeishuSignedAndGeneric(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("WH_URL", srv.URL)
	t.Setenv("WH_SECRET", "sec_123")
	sender := NewWebhookSender(config.AlertConfig{
		WebhookType:      "feishu",
		WebhookURLEnv:    "WH_URL",
		WebhookSecretEnv: "WH_SECRET",
	}, "", auth.NewRedactor())

	err := sender.Send(context.Background(), AlertPayload{
		EventType: "repo_alert",
		Title:     "Backup Alert",
		Message:   "repo failed",
	})
	if err != nil {
		t.Fatalf("Send feishu failed: %v", err)
	}
	if gotBody["sign"] == "" || gotBody["timestamp"] == "" || gotBody["msg_type"] != "text" {
		t.Fatalf("unexpected feishu payload: %+v", gotBody)
	}
}
