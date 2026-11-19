package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/muidea/git-backup/internal/config"
)

type ServiceOptions struct {
	ConfigPath      string
	SecretFile      string
	BinaryPath      string
	Interval        string
	UserMode        bool
	WithReport      bool
	DryRun          bool
	UnitDirOverride string
	SkipSystemctl   bool
}

type renderedUnit struct {
	Name    string
	Content string
}

const mainServiceTmpl = `[Unit]
Description=GitHub Repository Mirror Backup
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=-{{.SecretFile}}
ExecStart={{.BinaryPath}} sync --config {{.ConfigPath}}
SuccessExitStatus=0 2
IOSchedulingClass=best-effort
Nice=10

[Install]
WantedBy={{.WantedBy}}
`

const mainTimerTmpl = `[Unit]
Description=Run git-backup periodically

[Timer]
OnCalendar={{.Interval}}
RandomizedDelaySec=5m
Persistent=true

[Install]
WantedBy=timers.target
`

const reportServiceTmpl = `[Unit]
Description=Send weekly git-backup health report
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=-{{.SecretFile}}
ExecStart={{.BinaryPath}} report --weekly --config {{.ConfigPath}}

[Install]
WantedBy={{.WantedBy}}
`

const reportTimerTmpl = `[Unit]
Description=Send weekly git-backup health report

[Timer]
OnCalendar=Mon *-*-* 09:00:00
Persistent=true

[Install]
WantedBy=timers.target
`

// RunServiceInstall renders systemd unit files, writes them, and enables the timer(s).
func RunServiceInstall(ctx context.Context, opts ServiceOptions, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	resolved, err := resolveServiceOptions(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}

	units, err := renderSystemdUnits(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: render systemd units: %v\n", err)
		return ExitFatal
	}

	unitDir, err := systemdUnitDir(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}

	if resolved.DryRun {
		for _, u := range units {
			fmt.Fprintf(out, "=== %s ===\n%s\n", filepath.Join(unitDir, u.Name), u.Content)
		}
		return ExitOK
	}

	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: create unit directory %q (need root or --user?): %v\n", unitDir, err)
		return ExitFatal
	}

	for _, u := range units {
		p := filepath.Join(unitDir, u.Name)
		if err := os.WriteFile(p, []byte(u.Content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: write unit %q: %v\n", p, err)
			return ExitFatal
		}
		fmt.Fprintf(out, "Wrote %s\n", p)
	}

	if !resolved.SkipSystemctl {
		if err := runSystemctl(ctx, resolved.UserMode, out, "daemon-reload"); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: systemctl daemon-reload failed: %v\n", err)
			return ExitFatal
		}
		if err := runSystemctl(ctx, resolved.UserMode, out, "enable", "--now", "git-backup.timer"); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: systemctl enable git-backup.timer failed: %v\n", err)
			return ExitFatal
		}
		if resolved.WithReport {
			if err := runSystemctl(ctx, resolved.UserMode, out, "enable", "--now", "git-backup-report.timer"); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: systemctl enable git-backup-report.timer failed: %v\n", err)
				return ExitFatal
			}
		}
	}

	fmt.Fprintln(out, "Systemd service and timer installed successfully.")
	return ExitOK
}

// RunServiceUninstall disables timers and removes installed systemd unit files.
func RunServiceUninstall(ctx context.Context, opts ServiceOptions, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	unitDir, err := systemdUnitDir(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return ExitFatal
	}

	if !opts.DryRun && !opts.SkipSystemctl {
		for _, unit := range []string{"git-backup.timer", "git-backup-report.timer", "git-backup.service", "git-backup-report.service"} {
			_ = runSystemctl(ctx, opts.UserMode, io.Discard, "disable", "--now", unit)
		}
	}

	names := []string{"git-backup.service", "git-backup.timer", "git-backup-report.service", "git-backup-report.timer"}
	for _, name := range names {
		p := filepath.Join(unitDir, name)
		if opts.DryRun {
			fmt.Fprintf(out, "Would remove %s\n", p)
			continue
		}
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "ERROR: remove %q: %v\n", p, err)
				return ExitFatal
			}
		} else {
			fmt.Fprintf(out, "Removed %s\n", p)
		}
	}

	if !opts.DryRun && !opts.SkipSystemctl {
		_ = runSystemctl(ctx, opts.UserMode, out, "daemon-reload")
	}
	return ExitOK
}

// RunServiceStatus prints active timer and service status via systemctl.
func RunServiceStatus(ctx context.Context, userMode bool, out io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	if err := runSystemctl(ctx, userMode, out, "list-timers", "--all", "git-backup*"); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: systemctl list-timers failed: %v\n", err)
		return ExitFatal
	}
	_ = runSystemctl(ctx, userMode, out, "status", "--no-pager", "git-backup.service")
	return ExitOK
}

func resolveServiceOptions(opts ServiceOptions) (ServiceOptions, error) {
	if strings.TrimSpace(opts.Interval) == "" {
		opts.Interval = "0/4:00:00"
	}
	if strings.TrimSpace(opts.BinaryPath) == "" {
		exe, err := os.Executable()
		if err == nil && exe != "" {
			if resolved, linkErr := filepath.EvalSymlinks(exe); linkErr == nil {
				exe = resolved
			}
			opts.BinaryPath = exe
		} else {
			opts.BinaryPath = "/usr/local/bin/git-backup"
		}
	}
	cfgPath := strings.TrimSpace(opts.ConfigPath)
	if cfgPath == "" {
		discovered, err := config.DiscoverConfigPath("")
		if err != nil {
			return opts, err
		}
		cfgPath = discovered
	}
	absCfg, err := filepath.Abs(cfgPath)
	if err != nil {
		return opts, err
	}
	opts.ConfigPath = absCfg

	if strings.TrimSpace(opts.SecretFile) == "" {
		opts.SecretFile = filepath.Join(filepath.Dir(absCfg), "secret.env")
	} else {
		absSec, err := filepath.Abs(opts.SecretFile)
		if err != nil {
			return opts, err
		}
		opts.SecretFile = absSec
	}
	return opts, nil
}

func systemdUnitDir(opts ServiceOptions) (string, error) {
	if strings.TrimSpace(opts.UnitDirOverride) != "" {
		return opts.UnitDirOverride, nil
	}
	if opts.UserMode {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", fmt.Errorf("resolve user home dir for --user systemd units: %w", err)
		}
		return filepath.Join(home, ".config", "systemd", "user"), nil
	}
	return "/etc/systemd/system", nil
}

func renderSystemdUnits(opts ServiceOptions) ([]renderedUnit, error) {
	wantedBy := "multi-user.target"
	if opts.UserMode {
		wantedBy = "default.target"
	}
	data := struct {
		BinaryPath string
		ConfigPath string
		SecretFile string
		Interval   string
		WantedBy   string
	}{
		BinaryPath: opts.BinaryPath,
		ConfigPath: opts.ConfigPath,
		SecretFile: opts.SecretFile,
		Interval:   opts.Interval,
		WantedBy:   wantedBy,
	}

	render := func(name, tmplStr string) (renderedUnit, error) {
		t, err := template.New(name).Parse(tmplStr)
		if err != nil {
			return renderedUnit{}, err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return renderedUnit{}, err
		}
		return renderedUnit{Name: name, Content: buf.String()}, nil
	}

	u1, err := render("git-backup.service", mainServiceTmpl)
	if err != nil {
		return nil, err
	}
	u2, err := render("git-backup.timer", mainTimerTmpl)
	if err != nil {
		return nil, err
	}
	units := []renderedUnit{u1, u2}
	if opts.WithReport {
		u3, err := render("git-backup-report.service", reportServiceTmpl)
		if err != nil {
			return nil, err
		}
		u4, err := render("git-backup-report.timer", reportTimerTmpl)
		if err != nil {
			return nil, err
		}
		units = append(units, u3, u4)
	}
	return units, nil
}

func runSystemctl(ctx context.Context, userMode bool, out io.Writer, args ...string) error {
	fullArgs := make([]string, 0, len(args)+1)
	if userMode {
		fullArgs = append(fullArgs, "--user")
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, "systemctl", fullArgs...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}
