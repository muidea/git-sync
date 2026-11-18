package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/github"
)

const (
	ExitOK      = 0
	ExitFatal   = 1
	ExitPartial = 2
)

// Hooks allows tests to override external endpoints or clone URLs cleanly.
type Hooks struct {
	GitHubClientCustomizer func(*github.Client, *auth.TokenProvider)
	CloneURLOverride       func(meta github.RepoMeta) string
	SkipGitVersionCheck    bool
	SkipDiskSpaceCheck     bool
	NowFunc                func() time.Time
}

// NewLogger builds a structured slog.Logger configured from observability settings.
func NewLogger(obs config.ObservabilityConfig, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	var level slog.Level
	switch strings.ToLower(obs.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(obs.LogFormat) == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func cloneURLForMeta(meta github.RepoMeta, hooks *Hooks) string {
	if hooks != nil && hooks.CloneURLOverride != nil {
		if u := hooks.CloneURLOverride(meta); u != "" {
			return u
		}
	}
	if meta.CloneURL != "" {
		return meta.CloneURL
	}
	return "https://github.com/" + meta.FullName + ".git"
}

func currentTime(hooks *Hooks) time.Time {
	if hooks != nil && hooks.NowFunc != nil {
		return hooks.NowFunc().UTC()
	}
	return time.Now().UTC()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
