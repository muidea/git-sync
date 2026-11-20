package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/ledger"
)

// FormatPrometheus renders Prometheus exposition metrics from summary and ledger.
func FormatPrometheus(summary *ledger.StatusSummary, l *ledger.RepoLedger) []byte {
	var totalBytes int64
	if l != nil {
		for _, rec := range l.Repositories {
			totalBytes += rec.DiskSizeBytes
		}
	}

	var buf bytes.Buffer
	if summary != nil {
		buf.WriteString("# HELP git_backup_last_run_timestamp_seconds Unix timestamp of the last backup run completion.\n")
		buf.WriteString("# TYPE git_backup_last_run_timestamp_seconds gauge\n")
		fmt.Fprintf(&buf, "git_backup_last_run_timestamp_seconds %d\n", summary.FinishedAt.Unix())

		buf.WriteString("# HELP git_backup_duration_seconds Duration of the last backup run in seconds.\n")
		buf.WriteString("# TYPE git_backup_duration_seconds gauge\n")
		fmt.Fprintf(&buf, "git_backup_duration_seconds %.3f\n", float64(summary.DurationMS)/1000.0)

		buf.WriteString("# HELP git_backup_repos_total Number of repositories by state in the last run.\n")
		buf.WriteString("# TYPE git_backup_repos_total gauge\n")
		fmt.Fprintf(&buf, "git_backup_repos_total{status=\"cloned\"} %d\n", summary.Counts.Cloned)
		fmt.Fprintf(&buf, "git_backup_repos_total{status=\"updated\"} %d\n", summary.Counts.Updated)
		fmt.Fprintf(&buf, "git_backup_repos_total{status=\"up_to_date\"} %d\n", summary.Counts.UpToDate)
		fmt.Fprintf(&buf, "git_backup_repos_total{status=\"failed\"} %d\n", summary.Counts.Failed)
		fmt.Fprintf(&buf, "git_backup_repos_total{status=\"missing\"} %d\n", summary.Counts.Missing)
		fmt.Fprintf(&buf, "git_backup_repos_total{status=\"unmanaged\"} %d\n", summary.Counts.Unmanaged)
	}

	buf.WriteString("# HELP git_backup_storage_bytes_total Total disk bytes used across tracked repositories.\n")
	buf.WriteString("# TYPE git_backup_storage_bytes_total gauge\n")
	fmt.Fprintf(&buf, "git_backup_storage_bytes_total %d\n", totalBytes)
	return buf.Bytes()
}

// WritePrometheus atomically writes Prometheus textfile collector metrics.
func WritePrometheus(path string, summary *ledger.StatusSummary, l *ledger.RepoLedger) error {
	if strings.TrimSpace(path) == "" || summary == nil {
		return nil
	}
	return ledger.WriteFileAtomic(path, FormatPrometheus(summary, l), 0o644)
}

// AlertPayload represents an outgoing webhook notification.
type AlertPayload struct {
	EventType     string                  `json:"event_type"` // fatal | bulk_missing | repo_alert | recovery | weekly_report
	Title         string                  `json:"title"`
	Message       string                  `json:"message"`
	Timestamp     time.Time               `json:"timestamp"`
	Summary       *ledger.StatusSummary   `json:"summary,omitempty"`
	AbnormalRepos []ledger.AbnormalReport `json:"abnormal_repos,omitempty"`
}

// WebhookSender dispatches notifications to configured chat/generic webhooks.
type WebhookSender struct {
	webhookType string
	webhookURL  string
	secret      string
	httpClient  *http.Client
	redactor    *auth.Redactor
	nowFunc     func() time.Time
}

func NewWebhookSender(cfg config.AlertConfig, proxyURL string, redactor *auth.Redactor) *WebhookSender {
	if redactor == nil {
		redactor = auth.NewRedactor()
	}
	var urlVal, secVal string
	if cfg.WebhookURLEnv != "" {
		urlVal = strings.TrimSpace(os.Getenv(cfg.WebhookURLEnv))
		redactor.AddURL(urlVal, true)
	}
	if cfg.WebhookSecretEnv != "" {
		secVal = strings.TrimSpace(os.Getenv(cfg.WebhookSecretEnv))
		redactor.Add(secVal)
	}
	return &WebhookSender{
		webhookType: cfg.WebhookType,
		webhookURL:  urlVal,
		secret:      secVal,
		httpClient:  auth.HTTPClient(proxyURL, 15*time.Second),
		redactor:    redactor,
		nowFunc:     time.Now,
	}
}

// Enabled reports whether both webhook type and URL environment variable are configured.
func (s *WebhookSender) Enabled() bool {
	return s != nil && s.webhookType != "" && s.webhookURL != ""
}

// Send dispatches the alert payload formatted for the configured webhook_type.
func (s *WebhookSender) Send(ctx context.Context, payload AlertPayload) error {
	if !s.Enabled() {
		return nil
	}
	if payload.Timestamp.IsZero() {
		payload.Timestamp = s.nowFunc().UTC()
	}

	targetURL := s.webhookURL
	var bodyBytes []byte
	var err error

	switch s.webhookType {
	case "feishu":
		bodyBytes, err = s.buildFeishuBody(payload)
	case "dingtalk":
		targetURL, bodyBytes, err = s.buildDingTalkRequest(payload)
	case "wecom":
		bodyBytes, err = json.Marshal(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"content": fmt.Sprintf("### %s\n%s", payload.Title, payload.Message),
			},
		})
	case "slack":
		bodyBytes, err = json.Marshal(map[string]any{
			"text": fmt.Sprintf("*%s*\n%s", payload.Title, payload.Message),
		})
	case "generic":
		bodyBytes, err = json.Marshal(payload)
	default:
		return fmt.Errorf("unsupported webhook_type %q", s.webhookType)
	}
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build webhook request: %s", s.redactor.Redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send webhook: %s", s.redactor.Redact(err.Error()))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, s.redactor.Redact(string(respBody)))
	}
	return nil
}

func (s *WebhookSender) buildFeishuBody(payload AlertPayload) ([]byte, error) {
	body := map[string]any{
		"msg_type": "text",
		"content": map[string]string{
			"text": fmt.Sprintf("[%s]\n%s", payload.Title, payload.Message),
		},
	}
	if s.secret != "" {
		ts := strconv.FormatInt(s.nowFunc().Unix(), 10)
		sign := feishuSign(s.secret, ts)
		body["timestamp"] = ts
		body["sign"] = sign
	}
	return json.Marshal(body)
}

func (s *WebhookSender) buildDingTalkRequest(payload AlertPayload) (string, []byte, error) {
	targetURL := s.webhookURL
	if s.secret != "" {
		tsMillis := strconv.FormatInt(s.nowFunc().UnixMilli(), 10)
		sign := dingTalkSign(s.secret, tsMillis)
		u, err := url.Parse(targetURL)
		if err != nil {
			return "", nil, err
		}
		q := u.Query()
		q.Set("timestamp", tsMillis)
		q.Set("sign", sign)
		u.RawQuery = q.Encode()
		targetURL = u.String()
	}
	body, err := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": payload.Title,
			"text":  fmt.Sprintf("### %s\n%s", payload.Title, payload.Message),
		},
	})
	return targetURL, body, err
}

func feishuSign(secret, timestamp string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func dingTalkSign(secret, timestampMillis string) string {
	stringToSign := timestampMillis + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
