package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/muidea/git-backup/internal/app"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/server"
)

var version = "2.0.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return app.ExitFatal
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "serve":
		return cmdServe(ctx, rest)
	case "init":
		return cmdInit(ctx, rest)
	case "service":
		return cmdService(ctx, rest)
	case "sync":
		return cmdSync(ctx, rest)
	case "targets":
		return cmdTargets(ctx, rest)
	case "status":
		return cmdStatus(rest)
	case "verify":
		return cmdVerify(ctx, rest)
	case "report":
		return cmdReport(ctx, rest)
	case "version", "--version", "-v":
		fmt.Printf("git-backup v%s\n", version)
		return app.ExitOK
	case "help", "--help", "-h":
		printUsage()
		return app.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand %q\n\n", sub)
		printUsage()
		return app.ExitFatal
	}
}

func cmdInit(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Target config.yaml path")
	rootFlag := fs.String("root", "", "Backup storage directory")
	reposFlag := fs.String("repos", "", "Comma-separated repositories (owner/repo or GitHub URL)")
	authMode := fs.String("auth-mode", "", "Auth mode: pat or none")
	tokenFlag := fs.String("token", "", "GitHub PAT (optional, falls back to env or prompt)")
	tokenEnv := fs.String("token-env", "GITHUB_BACKUP_TOKEN", "Env var name for GitHub PAT")
	secretFile := fs.String("secret-file", "", "Path to secret.env (default <config_dir>/secret.env)")
	webhookType := fs.String("webhook-type", "", "Optional webhook type (feishu|wecom|dingtalk|slack|generic)")
	webhookURL := fs.String("webhook-url", "", "Optional webhook URL")
	force := fs.Bool("force", false, "Overwrite existing config.yaml")
	skipCheck := fs.Bool("skip-check", false, "Skip remote targets --check verification")
	installSvc := fs.Bool("install-service", false, "Install and enable systemd timer after init")
	userSvc := fs.Bool("user-service", false, "Install systemd units in --user scope")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}

	var repos []string
	if *reposFlag != "" {
		repos = append(repos, *reposFlag)
	}
	repos = append(repos, fs.Args()...)

	return app.RunInit(ctx, app.InitOptions{
		ConfigPath:     *cfgFlag,
		RootDir:        *rootFlag,
		Repos:          repos,
		AuthMode:       *authMode,
		Token:          *tokenFlag,
		TokenEnv:       *tokenEnv,
		SecretFile:     *secretFile,
		WebhookType:    *webhookType,
		WebhookURL:     *webhookURL,
		Force:          *force,
		SkipCheck:      *skipCheck,
		InstallService: *installSvc,
		UserService:    *userSvc,
	}, os.Stdout, nil)
}

func cmdService(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: git-backup service <install|uninstall|status> [flags]")
		return app.ExitFatal
	}
	action := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	secretFlag := fs.String("secret-file", "", "Path to secret.env (default <config_dir>/secret.env)")
	interval := fs.String("interval", "0/4:00:00", "systemd OnCalendar schedule")
	userMode := fs.Bool("user", false, "Install/manage systemd user units (~/.config/systemd/user)")
	withReport := fs.Bool("with-report", false, "Also install weekly report service and timer")
	dryRun := fs.Bool("dry-run", false, "Print generated systemd units without writing or enabling")
	if err := fs.Parse(rest); err != nil {
		return app.ExitFatal
	}

	opts := app.ServiceOptions{
		ConfigPath: *cfgFlag,
		SecretFile: *secretFlag,
		Interval:   *interval,
		UserMode:   *userMode,
		WithReport: *withReport,
		DryRun:     *dryRun,
	}

	switch action {
	case "install":
		return app.RunServiceInstall(ctx, opts, os.Stdout)
	case "uninstall":
		return app.RunServiceUninstall(ctx, opts, os.Stdout)
	case "status":
		return app.RunServiceStatus(ctx, *userMode, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "Unknown service action %q (expected install, uninstall, or status)\n", action)
		return app.ExitFatal
	}
}

func cmdServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	listenFlag := fs.String("listen", "", "HTTP listen address (overrides server.listen)")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}
	cfg, code := loadConfigOrFail(*cfgFlag)
	if code != app.ExitOK {
		return code
	}
	if strings.TrimSpace(*listenFlag) != "" {
		cfg.Server.Listen = strings.TrimSpace(*listenFlag)
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: invalid --listen: %v\n", err)
			return app.ExitFatal
		}
	}

	logger := app.NewLogger(cfg.Observability, os.Stderr)
	srv, err := server.New(cfg, logger, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return app.ExitFatal
	}
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: server exited with error: %v\n", err)
		return app.ExitFatal
	}
	return app.ExitOK
}

func cmdSync(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	repoFlag := fs.String("repo", "", "Optional comma-separated repository subset to sync")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}
	cfg, code := loadConfigOrFail(*cfgFlag)
	if code != app.ExitOK {
		return code
	}
	logger := app.NewLogger(cfg.Observability, os.Stderr)
	var filter []string
	for _, part := range strings.Split(*repoFlag, ",") {
		if p := strings.TrimSpace(part); p != "" {
			filter = append(filter, p)
		}
	}
	return app.RunSyncWithOptions(ctx, cfg, logger, app.SyncOptions{RepoFilter: filter}, nil).ExitCode
}

func cmdTargets(ctx context.Context, args []string) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub := args[0]
		rest := args[1:]
		switch sub {
		case "list":
			fs := flag.NewFlagSet("targets list", flag.ContinueOnError)
			cfgFlag := fs.String("config", "", "Path to config.yaml")
			if err := fs.Parse(rest); err != nil {
				return app.ExitFatal
			}
			cfg, code := loadConfigOrFail(*cfgFlag)
			if code != app.ExitOK {
				return code
			}
			return app.RunTargetsList(cfg, os.Stdout)
		case "add":
			fs := flag.NewFlagSet("targets add", flag.ContinueOnError)
			cfgFlag := fs.String("config", "", "Path to config.yaml")
			checkFlag := fs.Bool("check", true, "Verify remote reachability before adding")
			lfsFlag := fs.String("lfs", "", "Override enable_lfs (auto|true|false)")
			noPRRefs := fs.Bool("no-pr-refs", false, "Disable mirroring refs/pull/*")
			if err := fs.Parse(rest); err != nil {
				return app.ExitFatal
			}
			if fs.NArg() == 0 {
				fmt.Fprintln(os.Stderr, "Usage: git-backup targets add <owner/repo|url> [--config <path>] [--check=true]")
				return app.ExitFatal
			}
			cfg, code := loadConfigOrFail(*cfgFlag)
			if code != app.ExitOK {
				return code
			}
			tc := config.TargetConfig{Repo: fs.Arg(0)}
			if *lfsFlag != "" {
				mode := config.LFSMode(strings.ToLower(strings.TrimSpace(*lfsFlag)))
				tc.EnableLFS = &mode
			}
			if *noPRRefs {
				f := false
				tc.MirrorPRRefs = &f
			}
			return app.RunTargetsAdd(ctx, cfg, tc, *checkFlag, os.Stdout, nil)
		case "remove", "rm":
			fs := flag.NewFlagSet("targets remove", flag.ContinueOnError)
			cfgFlag := fs.String("config", "", "Path to config.yaml")
			if err := fs.Parse(rest); err != nil {
				return app.ExitFatal
			}
			if fs.NArg() == 0 {
				fmt.Fprintln(os.Stderr, "Usage: git-backup targets remove <owner/repo> [--config <path>]")
				return app.ExitFatal
			}
			cfg, code := loadConfigOrFail(*cfgFlag)
			if code != app.ExitOK {
				return code
			}
			return app.RunTargetsRemove(cfg, fs.Arg(0), os.Stdout)
		default:
			fmt.Fprintf(os.Stderr, "Unknown targets action %q (expected list, add, remove, or --check)\n", sub)
			return app.ExitFatal
		}
	}

	fs := flag.NewFlagSet("targets", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	checkFlag := fs.Bool("check", false, "Verify whitelist rules and remote reachability without modifying disk")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}
	if !*checkFlag {
		fmt.Fprintln(os.Stderr, "ERROR: specify --check, list, add, or remove")
		return app.ExitFatal
	}
	cfg, code := loadConfigOrFail(*cfgFlag)
	if code != app.ExitOK {
		return code
	}
	return app.RunTargetsCheck(ctx, cfg, os.Stdout, nil)
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	rootFlag := fs.String("root", "", "Backup root directory (optional, overrides config)")
	failedOnly := fs.Bool("failed", false, "Show only failed and missing repositories")
	staleStr := fs.String("stale", "", "Show repositories not synced within duration (e.g. 168h)")
	jsonOut := fs.Bool("json", false, "Output status as JSON")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}

	rootDir := *rootFlag
	if rootDir == "" {
		cfgPath, err := config.DiscoverConfigPath(*cfgFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v (or pass --root <dir>)\n", err)
			return app.ExitFatal
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			return app.ExitFatal
		}
		rootDir = cfg.Storage.RootDir
	}

	var staleAfter time.Duration
	if *staleStr != "" {
		d, err := time.ParseDuration(*staleStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: invalid --stale duration %q: %v\n", *staleStr, err)
			return app.ExitFatal
		}
		staleAfter = d
	}

	return app.RunStatus(app.StatusOptions{
		RootDir:    rootDir,
		FailedOnly: *failedOnly,
		StaleAfter: staleAfter,
		JSON:       *jsonOut,
	}, os.Stdout)
}

func cmdVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	runFsck := fs.Bool("fsck", false, "Run git fsck --no-dangling on verified repositories")
	jsonOut := fs.Bool("json", false, "Output verification results as JSON")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}
	cfg, code := loadConfigOrFail(*cfgFlag)
	if code != app.ExitOK {
		return code
	}
	return app.RunVerify(ctx, cfg, app.VerifyOptions{
		RepoFilter: app.ParseRepoFilter(fs.Args()),
		RunFsck:    *runFsck,
		JSON:       *jsonOut,
	}, os.Stdout, nil)
}

func cmdReport(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Path to config.yaml")
	weekly := fs.Bool("weekly", false, "Send weekly backup health summary via webhook")
	if err := fs.Parse(args); err != nil {
		return app.ExitFatal
	}
	if !*weekly {
		fmt.Fprintln(os.Stderr, "ERROR: specify --weekly to send the weekly health report")
		return app.ExitFatal
	}
	cfg, code := loadConfigOrFail(*cfgFlag)
	if code != app.ExitOK {
		return code
	}
	return app.RunReport(ctx, cfg, os.Stdout, nil)
}

func loadConfigOrFail(flagPath string) (*config.Config, int) {
	cfgPath, err := config.DiscoverConfigPath(flagPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return nil, app.ExitFatal
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "ERROR: config file not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: invalid config: %v\n", err)
		}
		return nil, app.ExitFatal
	}
	return cfg, app.ExitOK
}

func printUsage() {
	fmt.Print(`git-backup - Whitelist-driven GitHub repository mirror backup CLI

Usage:
  git-backup serve [--config <path>] [--listen 127.0.0.1:8080]
  git-backup init [--repos <r1,r2>] [--root <dir>] [--config <path>] [--install-service]
  git-backup service <install|uninstall|status> [--config <path>] [--user] [--with-report] [--dry-run]
  git-backup sync [--config <path>] [--repo <owner/repo>]
  git-backup targets <--check | list | add <repo> | remove <repo>> [--config <path>]
  git-backup status [--config <path> | --root <dir>] [--failed] [--stale 168h] [--json]
  git-backup verify [owner/repo] [--config <path>] [--fsck] [--json]
  git-backup report --weekly [--config <path>]
  git-backup version
`)
}
