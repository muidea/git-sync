package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/ledger"
	"github.com/muidea/git-backup/internal/notify"
)

// RunReport sends the weekly health summary via configured Webhook.
func RunReport(ctx context.Context, cfg *config.Config, out io.Writer, hooks *Hooks) int {
	if out == nil {
		out = os.Stdout
	}
	redactor := auth.NewRedactor()
	sender := notify.NewWebhookSender(cfg.Observability.Alert, cfg.Network.ProxyURL, redactor)
	if !sender.Enabled() {
		fmt.Fprintln(os.Stderr, "ERROR: webhook is not configured or webhook URL env is empty")
		return ExitFatal
	}

	repLedger, err := ledger.LoadLedger(cfg.Storage.RootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load repo_ledger.json: %v\n", err)
		return ExitFatal
	}
	summary, err := ledger.LoadSummary(cfg.Storage.RootDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ERROR: load status_summary.json: %v\n", err)
		return ExitFatal
	}

	now := currentTime(hooks)
	var totalBytes int64
	var abnormalLines []string
	var abnormalRepos []ledger.AbnormalReport
	var counts ledger.SummaryCounts

	for _, rec := range repLedger.Repositories {
		totalBytes += rec.DiskSizeBytes
		switch rec.State {
		case ledger.StatusCloned:
			counts.Cloned++
			counts.ManagedTotal++
		case ledger.StatusUpdated:
			counts.Updated++
			counts.ManagedTotal++
		case ledger.StatusUpToDate:
			counts.UpToDate++
			counts.ManagedTotal++
		case ledger.StatusFailed:
			counts.Failed++
			counts.ManagedTotal++
			abnormalRepos = append(abnormalRepos, ledger.AbnormalReport{
				Repo:                rec.FullName,
				State:               rec.State,
				ConsecutiveFailures: rec.ConsecutiveFailures,
				LastError:           rec.LastError,
			})
			abnormalLines = append(abnormalLines, fmt.Sprintf("- `%s` [failed x%d]: %s", rec.FullName, rec.ConsecutiveFailures, rec.LastError))
		case ledger.StatusMissing:
			counts.Missing++
			counts.ManagedTotal++
			abnormalRepos = append(abnormalRepos, ledger.AbnormalReport{
				Repo:                rec.FullName,
				State:               rec.State,
				ConsecutiveFailures: rec.ConsecutiveFailures,
				LastError:           rec.LastError,
			})
			abnormalLines = append(abnormalLines, fmt.Sprintf("- `%s` [missing, 本地保留]: %s", rec.FullName, rec.LastError))
		case ledger.StatusUnmanaged:
			counts.Unmanaged++
		}
	}

	var lines []string
	if summary != nil {
		age := now.Sub(summary.FinishedAt).Round(time.Minute)
		lines = append(lines, fmt.Sprintf("最近同步时间：%s（距今 %s）", summary.FinishedAt.UTC().Format("2006-01-02 15:04:05 UTC"), age))
		if age > 24*time.Hour {
			lines = append(lines, "⚠️ 警告：最近一次同步已超过 24 小时，请检查定时器是否正常运行。")
		}
	} else {
		lines = append(lines, "⚠️ 尚未检测到 `status_summary.json` 同步记录。")
	}

	lines = append(lines, fmt.Sprintf("纳管总数：%d | 正常：%d | 失败：%d | 缺失(保留)：%d | 失管：%d | 总磁盘占用：%s",
		counts.ManagedTotal,
		counts.Cloned+counts.Updated+counts.UpToDate,
		counts.Failed,
		counts.Missing,
		counts.Unmanaged,
		formatBytes(totalBytes),
	))
	if len(abnormalLines) > 0 {
		lines = append(lines, "异常明细：\n"+strings.Join(abnormalLines, "\n"))
	}

	payload := notify.AlertPayload{
		EventType:     "weekly_report",
		Title:         "git-backup 每周健康度周报",
		Message:       strings.Join(lines, "\n"),
		Timestamp:     now,
		Summary:       summary,
		AbnormalRepos: abnormalRepos,
	}
	if err := sender.Send(ctx, payload); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: send weekly report failed: %s\n", redactor.Redact(err.Error()))
		return ExitFatal
	}
	fmt.Fprintln(out, "Weekly report sent successfully.")
	return ExitOK
}
