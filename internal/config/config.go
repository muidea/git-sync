package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigEnv = "GIT_BACKUP_CONFIG"
	SystemConfigPath = "/etc/git-backup/config.yaml"
)

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// LFSMode represents how Git LFS syncing is configured for a repository.
type LFSMode string

const (
	LFSAuto  LFSMode = "auto"
	LFSTrue  LFSMode = "true"
	LFSFalse LFSMode = "false"
)

// UnmarshalYAML supports both boolean (true/false) and string ("true"/"false"/"auto") values.
func (m *LFSMode) UnmarshalYAML(value *yaml.Node) error {
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			*m = LFSTrue
		} else {
			*m = LFSFalse
		}
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("enable_lfs must be boolean or string (true|false|auto): %w", err)
	}
	s = strings.ToLower(strings.TrimSpace(s))
	switch LFSMode(s) {
	case LFSAuto, LFSTrue, LFSFalse:
		*m = LFSMode(s)
		return nil
	default:
		return fmt.Errorf("invalid enable_lfs value %q: expected true, false, or auto", s)
	}
}

// Duration wraps time.Duration for clean YAML parsing.
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses human-readable Go duration strings (e.g. "15m", "4h").
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Config defines the declarative configuration schema for git-backup.
type Config struct {
	Storage       StorageConfig       `yaml:"storage"`
	Auth          AuthConfig          `yaml:"auth"`
	Defaults      DefaultsConfig      `yaml:"defaults"`
	Targets       []TargetConfig      `yaml:"targets"`
	Sync          SyncConfig          `yaml:"sync"`
	Network       NetworkConfig       `yaml:"network"`
	Safety        SafetyConfig        `yaml:"safety"`
	Observability ObservabilityConfig `yaml:"observability"`
}

type StorageConfig struct {
	RootDir            string  `yaml:"root_dir"`
	TrashRetentionDays int     `yaml:"trash_retention_days"`
	GCIntervalSyncs    int     `yaml:"gc_interval_syncs"`
	MinFreeDiskPercent float64 `yaml:"min_free_disk_percent"`
	MinFreeDiskGB      float64 `yaml:"min_free_disk_gb"`
}

type AuthConfig struct {
	Mode           string `yaml:"mode"`
	TokenEnv       string `yaml:"token_env"`
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

type DefaultsConfig struct {
	EnableLFS       LFSMode  `yaml:"enable_lfs"`
	MirrorPRRefs    *bool    `yaml:"mirror_pr_refs"`
	CloneTimeout    Duration `yaml:"clone_timeout"`
	UpdateTimeout   Duration `yaml:"update_timeout"`
	UnmanagedPolicy string   `yaml:"unmanaged_policy"`
}

type TargetConfig struct {
	Repo          string   `yaml:"repo"`
	EnableLFS     *LFSMode `yaml:"enable_lfs,omitempty"`
	MirrorPRRefs  *bool    `yaml:"mirror_pr_refs,omitempty"`
	CloneTimeout  Duration `yaml:"clone_timeout,omitempty"`
	UpdateTimeout Duration `yaml:"update_timeout,omitempty"`
	SkipUnchanged *bool    `yaml:"skip_unchanged,omitempty"`
}

// ResolvedTarget holds the effective merged settings for a single whitelisted repository.
type ResolvedTarget struct {
	Repo          string
	Owner         string
	Name          string
	CanonicalKey  string
	EnableLFS     LFSMode
	MirrorPRRefs  bool
	CloneTimeout  time.Duration
	UpdateTimeout time.Duration
	SkipUnchanged bool
}

type SyncConfig struct {
	Concurrency       int  `yaml:"concurrency"`
	SkipUnchanged     bool `yaml:"skip_unchanged"`
	ForceFullSyncDays int  `yaml:"force_full_sync_days"`
	MaxRetries        int  `yaml:"max_retries"`
}

type NetworkConfig struct {
	ProxyURL      string `yaml:"proxy_url"`
	HTTPVersion   string `yaml:"http_version"`
	LowSpeedLimit int    `yaml:"low_speed_limit"`
	LowSpeedTime  int    `yaml:"low_speed_time"`
}

type SafetyConfig struct {
	MaxMissingAlertRatio float64 `yaml:"max_missing_alert_ratio"`
}

type ObservabilityConfig struct {
	LogFormat              string      `yaml:"log_format"`
	LogLevel               string      `yaml:"log_level"`
	PrometheusTextfilePath string      `yaml:"prometheus_textfile_path"`
	Alert                  AlertConfig `yaml:"alert"`
}

type AlertConfig struct {
	WebhookType      string   `yaml:"webhook_type"`
	WebhookURLEnv    string   `yaml:"webhook_url_env"`
	WebhookSecretEnv string   `yaml:"webhook_secret_env"`
	FailureThreshold int      `yaml:"failure_threshold"`
	AlertCooldown    Duration `yaml:"alert_cooldown"`
}

// UserConfigPath returns ~/.config/git-backup/config.yaml if a home directory can be resolved.
func UserConfigPath() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".config", "git-backup", "config.yaml")
	}
	return ""
}

// NormalizeRepoName converts owner/repo, HTTPS URLs, or SSH URLs into validated owner/repo format.
func NormalizeRepoName(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	for _, prefix := range []string{
		"git@github.com:",
		"ssh://git@github.com/",
		"https://github.com/",
		"http://github.com/",
	} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			s = s[len(prefix):]
			break
		}
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	if !repoNamePattern.MatchString(s) {
		return "", fmt.Errorf("invalid repository %q: expected owner/repo or GitHub URL", raw)
	}
	return s, nil
}

// DiscoverConfigPath resolves the config file path using the documented precedence:
// --config flag > GIT_BACKUP_CONFIG env > ./config.yaml > ~/.config/git-backup/config.yaml > /etc/git-backup/config.yaml.
func DiscoverConfigPath(flagPath string) (string, error) {
	if strings.TrimSpace(flagPath) != "" {
		return flagPath, nil
	}
	if envPath := strings.TrimSpace(os.Getenv(DefaultConfigEnv)); envPath != "" {
		return envPath, nil
	}
	candidates := []string{"./config.yaml"}
	if userCfg := UserConfigPath(); userCfg != "" {
		candidates = append(candidates, userCfg)
	}
	candidates = append(candidates, SystemConfigPath)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("config file not found; specify --config, set %s, or run `git-backup init`", DefaultConfigEnv)
}

// Load reads, applies defaults to, and validates the YAML configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	mirrorPR := true
	return &Config{
		Storage: StorageConfig{
			TrashRetentionDays: 30,
			GCIntervalSyncs:    25,
			MinFreeDiskPercent: 10,
			MinFreeDiskGB:      20,
		},
		Auth: AuthConfig{
			Mode:     "pat",
			TokenEnv: "GITHUB_BACKUP_TOKEN",
		},
		Defaults: DefaultsConfig{
			EnableLFS:       LFSAuto,
			MirrorPRRefs:    &mirrorPR,
			CloneTimeout:    Duration{Duration: time.Hour},
			UpdateTimeout:   Duration{Duration: 15 * time.Minute},
			UnmanagedPolicy: "retain",
		},
		Sync: SyncConfig{
			Concurrency:       6,
			SkipUnchanged:     true,
			ForceFullSyncDays: 7,
			MaxRetries:        3,
		},
		Network: NetworkConfig{
			HTTPVersion:   "HTTP/1.1",
			LowSpeedLimit: 1000,
			LowSpeedTime:  60,
		},
		Safety: SafetyConfig{
			MaxMissingAlertRatio: 0.30,
		},
		Observability: ObservabilityConfig{
			LogFormat: "json",
			LogLevel:  "info",
			Alert: AlertConfig{
				FailureThreshold: 2,
				AlertCooldown:    Duration{Duration: 24 * time.Hour},
			},
		},
	}
}

// Validate verifies all required fields, value ranges, and target whitelist invariants.
func (c *Config) Validate() error {
	c.Storage.RootDir = strings.TrimSpace(c.Storage.RootDir)
	if c.Storage.RootDir == "" {
		return errors.New("storage.root_dir is required")
	}
	absRoot, err := filepath.Abs(c.Storage.RootDir)
	if err != nil {
		return fmt.Errorf("invalid storage.root_dir %q: %w", c.Storage.RootDir, err)
	}
	c.Storage.RootDir = absRoot

	if c.Storage.TrashRetentionDays < 1 {
		return errors.New("storage.trash_retention_days must be >= 1")
	}
	if c.Storage.GCIntervalSyncs < 0 {
		return errors.New("storage.gc_interval_syncs must be >= 0")
	}
	if c.Storage.MinFreeDiskPercent < 0 || c.Storage.MinFreeDiskPercent >= 100 {
		return errors.New("storage.min_free_disk_percent must be in [0, 100)")
	}
	if c.Storage.MinFreeDiskGB < 0 {
		return errors.New("storage.min_free_disk_gb must be >= 0")
	}

	switch strings.ToLower(strings.TrimSpace(c.Auth.Mode)) {
	case "pat":
		c.Auth.Mode = "pat"
		if strings.TrimSpace(c.Auth.TokenEnv) == "" {
			return errors.New("auth.token_env is required when auth.mode=pat")
		}
	case "github_app":
		c.Auth.Mode = "github_app"
		if c.Auth.AppID <= 0 || c.Auth.InstallationID <= 0 || strings.TrimSpace(c.Auth.PrivateKeyPath) == "" {
			return errors.New("auth.app_id, auth.installation_id, and auth.private_key_path are required when auth.mode=github_app")
		}
	case "none":
		c.Auth.Mode = "none"
	default:
		return fmt.Errorf("unsupported auth.mode %q: expected pat, github_app, or none", c.Auth.Mode)
	}

	switch c.Defaults.UnmanagedPolicy {
	case "retain", "archive":
	default:
		return fmt.Errorf("invalid defaults.unmanaged_policy %q: expected retain or archive", c.Defaults.UnmanagedPolicy)
	}

	if c.Defaults.CloneTimeout.Duration <= 0 || c.Defaults.UpdateTimeout.Duration <= 0 {
		return errors.New("defaults.clone_timeout and defaults.update_timeout must be > 0")
	}

	if len(c.Targets) == 0 {
		return errors.New("targets whitelist must not be empty")
	}
	seen := make(map[string]struct{}, len(c.Targets))
	for i := range c.Targets {
		repo := strings.TrimSpace(c.Targets[i].Repo)
		if !repoNamePattern.MatchString(repo) {
			return fmt.Errorf("targets[%d].repo %q must be in owner/repo format", i, c.Targets[i].Repo)
		}
		key := strings.ToLower(repo)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("duplicate repository %q in targets whitelist", repo)
		}
		seen[key] = struct{}{}
		c.Targets[i].Repo = repo
		if c.Targets[i].CloneTimeout.Duration < 0 || c.Targets[i].UpdateTimeout.Duration < 0 {
			return fmt.Errorf("targets[%d] timeout must not be negative", i)
		}
	}

	if c.Sync.Concurrency < 1 {
		return errors.New("sync.concurrency must be >= 1")
	}
	if c.Sync.ForceFullSyncDays < 0 {
		return errors.New("sync.force_full_sync_days must be >= 0")
	}
	if c.Sync.MaxRetries < 0 {
		return errors.New("sync.max_retries must be >= 0")
	}

	switch strings.ToUpper(strings.TrimSpace(c.Network.HTTPVersion)) {
	case "", "AUTO":
		c.Network.HTTPVersion = ""
	case "HTTP/1.1":
		c.Network.HTTPVersion = "HTTP/1.1"
	case "HTTP/2":
		c.Network.HTTPVersion = "HTTP/2"
	default:
		return fmt.Errorf("invalid network.http_version %q: expected HTTP/1.1, HTTP/2, or auto", c.Network.HTTPVersion)
	}
	if c.Network.LowSpeedLimit < 0 || c.Network.LowSpeedTime < 0 {
		return errors.New("network.low_speed_limit and network.low_speed_time must be >= 0")
	}

	if c.Safety.MaxMissingAlertRatio <= 0 || c.Safety.MaxMissingAlertRatio > 1 {
		return errors.New("safety.max_missing_alert_ratio must be in (0, 1]")
	}

	switch strings.ToLower(c.Observability.LogFormat) {
	case "json", "text":
		c.Observability.LogFormat = strings.ToLower(c.Observability.LogFormat)
	default:
		return fmt.Errorf("invalid observability.log_format %q: expected json or text", c.Observability.LogFormat)
	}

	switch strings.ToLower(c.Observability.LogLevel) {
	case "debug", "info", "warn", "error":
		c.Observability.LogLevel = strings.ToLower(c.Observability.LogLevel)
	default:
		return fmt.Errorf("invalid observability.log_level %q", c.Observability.LogLevel)
	}

	if wt := strings.ToLower(strings.TrimSpace(c.Observability.Alert.WebhookType)); wt != "" {
		switch wt {
		case "feishu", "wecom", "dingtalk", "slack", "generic":
			c.Observability.Alert.WebhookType = wt
		default:
			return fmt.Errorf("invalid observability.alert.webhook_type %q", c.Observability.Alert.WebhookType)
		}
		if strings.TrimSpace(c.Observability.Alert.WebhookURLEnv) == "" {
			return errors.New("observability.alert.webhook_url_env is required when webhook_type is set")
		}
	}
	if c.Observability.Alert.FailureThreshold < 1 {
		return errors.New("observability.alert.failure_threshold must be >= 1")
	}

	return nil
}

// ResolveTargets merges global defaults with per-target overrides.
func (c *Config) ResolveTargets() []ResolvedTarget {
	out := make([]ResolvedTarget, 0, len(c.Targets))
	defaultMirrorPR := true
	if c.Defaults.MirrorPRRefs != nil {
		defaultMirrorPR = *c.Defaults.MirrorPRRefs
	}

	for _, t := range c.Targets {
		parts := strings.SplitN(t.Repo, "/", 2)
		rt := ResolvedTarget{
			Repo:          t.Repo,
			Owner:         parts[0],
			Name:          parts[1],
			CanonicalKey:  strings.ToLower(t.Repo),
			EnableLFS:     c.Defaults.EnableLFS,
			MirrorPRRefs:  defaultMirrorPR,
			CloneTimeout:  c.Defaults.CloneTimeout.Duration,
			UpdateTimeout: c.Defaults.UpdateTimeout.Duration,
			SkipUnchanged: c.Sync.SkipUnchanged,
		}
		if t.EnableLFS != nil {
			rt.EnableLFS = *t.EnableLFS
		}
		if t.MirrorPRRefs != nil {
			rt.MirrorPRRefs = *t.MirrorPRRefs
		}
		if t.CloneTimeout.Duration > 0 {
			rt.CloneTimeout = t.CloneTimeout.Duration
		}
		if t.UpdateTimeout.Duration > 0 {
			rt.UpdateTimeout = t.UpdateTimeout.Duration
		}
		if t.SkipUnchanged != nil {
			rt.SkipUnchanged = *t.SkipUnchanged
		}
		out = append(out, rt)
	}
	return out
}
