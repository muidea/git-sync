package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/ledger"
)

type StatusOptions struct {
	RootDir    string
	FailedOnly bool
	StaleAfter time.Duration
	JSON       bool
	Now        time.Time
}

// RunStatus renders the latest summary and repository ledger state to out.
func RunStatus(opts StatusOptions, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	repLedger, err := ledger.LoadLedger(opts.RootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load repo_ledger.json: %v\n", err)
		return ExitFatal
	}
	summary, sumErr := ledger.LoadSummary(opts.RootDir)
	if sumErr != nil && !errors.Is(sumErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ERROR: load status_summary.json: %v\n", sumErr)
		return ExitFatal
	}

	records := make([]*ledger.RepoRecord, 0, len(repLedger.Repositories))
	for _, rec := range repLedger.Repositories {
		if opts.FailedOnly && rec.State != ledger.StatusFailed && rec.State != ledger.StatusMissing {
			continue
		}
		if opts.StaleAfter > 0 {
			if rec.LastGitSyncAt != nil && opts.Now.Sub(*rec.LastGitSyncAt) <= opts.StaleAfter {
				continue
			}
		}
		records = append(records, rec)
	}

	sort.Slice(records, func(i, j int) bool {
		pi, pj := statusSortPriority(records[i].State), statusSortPriority(records[j].State)
		if pi != pj {
			return pi < pj
		}
		ti, tj := time.Time{}, time.Time{}
		if records[i].LastGitSyncAt != nil {
			ti = *records[i].LastGitSyncAt
		}
		if records[j].LastGitSyncAt != nil {
			tj = *records[j].LastGitSyncAt
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return records[i].FullName < records[j].FullName
	})

	if opts.JSON {
		payload := map[string]any{
			"summary":      summary,
			"repositories": records,
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: encode json: %v\n", err)
			return ExitFatal
		}
		return ExitOK
	}

	if summary != nil {
		resultLabel := "SUCCESS (Exit 0)"
		if summary.ExitCode == ExitPartial {
			resultLabel = "PARTIAL_FAILURE (Exit 2)"
		} else if summary.ExitCode == ExitFatal {
			resultLabel = "FATAL (Exit 1)"
		}
		fmt.Fprintf(out, "=== Last Backup Summary (Run ID: %s) ===\n", summary.RunID)
		fmt.Fprintf(out, "Started: %s | Duration: %.1fs | Result: %s\n",
			summary.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
			float64(summary.DurationMS)/1000.0,
			resultLabel,
		)
		fmt.Fprintf(out, "Total Managed: %d | Cloned: %d | Updated: %d | Up-to-date: %d | Failed: %d | Missing(Retained): %d | Unmanaged: %d\n\n",
			summary.Counts.ManagedTotal,
			summary.Counts.Cloned,
			summary.Counts.Updated,
			summary.Counts.UpToDate,
			summary.Counts.Failed,
			summary.Counts.Missing,
			summary.Counts.Unmanaged,
		)
	} else {
		fmt.Fprintf(out, "=== Backup Status (no status_summary.json found yet) ===\n\n")
	}

	var abnormal, normal []*ledger.RepoRecord
	for _, r := range records {
		if r.State == ledger.StatusFailed || r.State == ledger.StatusMissing {
			abnormal = append(abnormal, r)
		} else {
			normal = append(normal, r)
		}
	}

	if len(abnormal) > 0 {
		fmt.Fprintf(out, "[!] ABNORMAL REPOSITORIES (%d)\n", len(abnormal))
		fmt.Fprintf(out, "%-30s %-8s %-6s %-21s %s\n", "REPO", "STATE", "FAILS", "LAST SUCCESS", "DETAIL / ERROR SUMMARY")
		for _, r := range abnormal {
			lastSuccess := "-"
			if r.LastGitSyncAt != nil {
				lastSuccess = r.LastGitSyncAt.UTC().Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(out, "%-30s %-8s %-6d %-21s %s\n", r.FullName, r.State, r.ConsecutiveFailures, lastSuccess, r.LastError)
		}
		fmt.Fprintln(out)
	}

	if !opts.FailedOnly {
		fmt.Fprintf(out, "[*] REPOSITORIES (%d)\n", len(normal))
		fmt.Fprintf(out, "%-30s %-11s %-9s %-21s %-10s %s\n", "REPO", "STATUS", "HEAD", "LAST SYNC", "DISK SIZE", "LFS")
		for _, r := range normal {
			head := shortSHA(r.HeadCommit)
			lastSync := "-"
			if r.LastGitSyncAt != nil {
				lastSync = r.LastGitSyncAt.UTC().Format("2006-01-02 15:04:05")
			}
			lfsFlag := "off"
			if r.LFSEnabled {
				lfsFlag = "on"
			}
			fmt.Fprintf(out, "%-30s %-11s %-9s %-21s %-10s %s\n",
				r.FullName, r.State, head, lastSync, formatBytes(r.DiskSizeBytes), lfsFlag,
			)
		}
	}
	return ExitOK
}

func statusSortPriority(s ledger.RepoStatus) int {
	switch s {
	case ledger.StatusFailed:
		return 0
	case ledger.StatusMissing:
		return 1
	case ledger.StatusCloned, ledger.StatusUpdated:
		return 2
	case ledger.StatusUpToDate:
		return 3
	default:
		return 4
	}
}

func shortSHA(sha string) string {
	if sha == "" {
		return "-"
	}
	if sha == "EMPTY" || len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

func formatBytes(b int64) string {
	const unit = 1024.0
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	val := float64(b)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		val /= unit
		if val < unit || suffix == "TB" {
			return fmt.Sprintf("%.1f %s", val, suffix)
		}
	}
	return fmt.Sprintf("%d B", b)
}

func ParseRepoFilter(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.TrimSpace(args[0])
}
