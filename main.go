// prreview — an interactive terminal UI that reviews GitHub pull requests
// with an LLM (Anthropic API or any OpenAI-compatible endpoint) and lets you
// approve, request changes, or comment. Also ships a PM2-style watch daemon.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"prreview/internal/ai"
	"prreview/internal/config"
	"prreview/internal/tui"
	"prreview/internal/watch"
)

const usage = `prreview — AI-assisted GitHub PR review & approve

Usage:
  prreview <PR_URL> [-m <name>]    review and approve (interactive)
  prreview review <PR_URL>         review only
  prreview approve <PR_URL>        approve (still shows AI findings first)

  prreview watch [flags]           dashboard: poll review requests, auto-review
      --daemon                     run headless in the background instead
      --interval <sec>             poll interval (default 15)
      --auto-approve               approve clean PRs automatically
      --concurrency <n>            parallel reviews (default 2)
      --repo <owner/name>          whitelist repos (repeatable; empty = all)
      --include-teams              also pick up team review requests
                                   (default: only direct requests to you)
      -m, --model <name>           use a model profile for this run
  prreview watch stop|status       manage the background daemon
  prreview watch logs [-f]         show (follow) daemon logs

  prreview serve [flags]           web UI (default http://127.0.0.1:8787)
      --port <n>                   port to listen on (default 8787)
      --detach, -d                 run the web UI in the background
      --interval / --auto-approve / --concurrency / -m as with watch
      --no-open                    don't open the browser automatically
  prreview serve stop|status       manage the detached web UI
  prreview serve logs [-f]         show (follow) web UI logs
  prreview purge [--port <n>]      clear the serve queue (keeps running/awaiting)

  prreview model                   manage model profiles (interactive)
  prreview model list              list profiles
  prreview model use <name>        set the default model
  prreview model test [name]       test key/endpoint/model connectivity
  prreview model discover [name]   list models available at the endpoint
  prreview model rm <name>         remove a profile

  prreview config                  interactive settings (depth, fallback model)
  prreview config show             print current config (key masked)
  prreview config set <key> <val>  api_key | provider | base_url | model |
                                   max_tokens | effort | depth | default_model

Config keys:
  provider   anthropic (default) | openai (self-hosted: vLLM/Ollama/LiteLLM/DeepSeek)
  base_url   custom endpoint, e.g. https://llm.internal.example.com
  depth      fast | standard | max (default max — full multi-pass pipeline)
  effort     low|medium|high|xhigh|max (Anthropic reasoning depth override)

Environment:
  PRREVIEW_API_KEY / ANTHROPIC_API_KEY override the stored API key.
  PRREVIEW_BASE_URL overrides the endpoint.

Requires the gh CLI, authenticated (gh auth login).`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// extractModelFlag removes -m/--model <name> from args and returns the name.
func extractModelFlag(args []string) (rest []string, model string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" || args[i] == "--model" {
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s needs a profile name", args[i])
			}
			model = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return rest, model, nil
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return nil
	}

	switch args[0] {
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	case "config":
		return runConfig(args[1:])
	case "model", "models":
		return runModel(args[1:])
	case "watch":
		return runWatch(args[1:])
	case "serve", "ui", "web":
		return runServe(args[1:])
	case "purge":
		return runPurge(args[1:])
	case "__watch-run": // hidden: daemon child process
		return runWatchHeadless(args[1:])
	case "__serve-run": // hidden: serve daemon child process
		return runServeMain(args[1:], true)
	case "review":
		return runReview(args[1:], tui.ModeReview)
	case "approve":
		return runReview(args[1:], tui.ModeApprove)
	default:
		if strings.Contains(args[0], "github.com/") {
			return runReview(args, tui.ModeReviewAndApprove)
		}
		fmt.Println(usage)
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func loadResolved(modelName string) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	resolved, err := cfg.Resolved(modelName)
	if err != nil {
		return nil, err
	}
	if resolved.APIKey == "" {
		return nil, fmt.Errorf("no API key configured — run: prreview config (or prreview model)")
	}
	return resolved, nil
}

func runReview(args []string, mode tui.Mode) error {
	args, modelName, err := extractModelFlag(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("missing PR URL — usage: prreview %s <PR_URL>", mode)
	}
	cfg, err := loadResolved(modelName)
	if err != nil {
		return err
	}
	p := tea.NewProgram(tui.New(cfg, args[0], mode), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func parseWatchOpts(args []string) (watch.Options, string, bool, error) {
	opts := watch.Options{Interval: 15 * time.Second, Concurrency: 2}
	daemon := false
	args, modelName, err := extractModelFlag(args)
	if err != nil {
		return opts, "", false, err
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--daemon":
			daemon = true
		case "--auto-approve":
			opts.AutoApprove = true
		case "--include-teams":
			opts.IncludeTeams = true
		case "--repo":
			i++
			if i >= len(args) {
				return opts, "", false, fmt.Errorf("--repo needs owner/name")
			}
			opts.Repos = append(opts.Repos, args[i])
		case "--interval":
			i++
			if i >= len(args) {
				return opts, "", false, fmt.Errorf("--interval needs a value (seconds)")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 5 {
				return opts, "", false, fmt.Errorf("--interval must be an integer >= 5 (seconds)")
			}
			opts.Interval = time.Duration(n) * time.Second
		case "--concurrency":
			i++
			if i >= len(args) {
				return opts, "", false, fmt.Errorf("--concurrency needs a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > 8 {
				return opts, "", false, fmt.Errorf("--concurrency must be 1-8")
			}
			opts.Concurrency = n
		default:
			return opts, "", false, fmt.Errorf("unknown watch flag: %s", args[i])
		}
	}
	return opts, modelName, daemon, nil
}

func runWatch(args []string) error {
	// daemon management subcommands
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			return stopDaemon("watch")
		case "status":
			return daemonStatus("watch")
		case "logs":
			follow := len(args) > 1 && (args[1] == "-f" || args[1] == "--follow")
			return daemonLogs("watch", follow)
		}
	}

	opts, modelName, daemon, err := parseWatchOpts(args)
	if err != nil {
		return err
	}
	if daemon {
		// validate config before spawning so failures are visible here
		if _, err := loadResolved(modelName); err != nil {
			return err
		}
		var childArgs []string
		for _, a := range args {
			if a != "--daemon" {
				childArgs = append(childArgs, a)
			}
		}
		pid, err := startDaemon("watch", "__watch-run", childArgs)
		if err != nil {
			return err
		}
		printDaemonHints("watch", pid)
		return nil
	}

	cfg, err := loadResolved(modelName)
	if err != nil {
		return err
	}
	applyWatchDefaults(&opts, cfg)
	model, err := watch.New(cfg, opts)
	if err != nil {
		return err
	}
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// runWatchHeadless is the daemon child entry point (stdout/stderr already
// redirected to the log file by the parent).
func runWatchHeadless(args []string) error {
	opts, modelName, _, err := parseWatchOpts(args)
	if err != nil {
		return err
	}
	cfg, err := loadResolved(modelName)
	if err != nil {
		return err
	}
	applyWatchDefaults(&opts, cfg)
	return watch.RunHeadless(cfg, opts, os.Stdout)
}

// applyWatchDefaults fills unset watch filters from the config file; CLI
// flags win, and repos are normalized.
func applyWatchDefaults(opts *watch.Options, cfg *config.Config) {
	if !opts.IncludeTeams {
		opts.IncludeTeams = cfg.WatchIncludeTeams
	}
	if len(opts.Repos) == 0 {
		opts.Repos = cfg.WatchRepos
	}
	if normalized, err := config.NormalizeRepos(opts.Repos); err == nil {
		opts.Repos = normalized
	}
}

func runModel(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(args) == 0 || args[0] == "add" || args[0] == "edit" {
		p := tea.NewProgram(tui.NewModelPicker(cfg))
		_, err := p.Run()
		return err
	}

	switch args[0] {
	case "list", "ls":
		if len(cfg.Models) == 0 {
			fmt.Println("no model profiles — run: prreview model")
			return nil
		}
		names := make([]string, 0, len(cfg.Models))
		for n := range cfg.Models {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := cfg.Models[n]
			def := " "
			if n == cfg.DefaultModelName {
				def = "*"
			}
			endpoint := p.BaseURL
			if endpoint == "" {
				endpoint = "(default endpoint)"
			}
			fmt.Printf("%s %-14s %-10s %-28s %s\n", def, n, p.Provider, p.Model, endpoint)
		}
		return nil
	case "discover":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		resolved, err := cfg.Resolved(name)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		models, err := ai.ListModels(ctx, resolved)
		if err != nil {
			return err
		}
		for _, m := range models {
			fmt.Println(m)
		}
		return nil
	case "test":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		resolved, err := cfg.Resolved(name)
		if err != nil {
			return err
		}
		if resolved.APIKey == "" {
			return fmt.Errorf("no API key configured — run: prreview config (or prreview model)")
		}
		fmt.Printf("testing %s (%s) ... ", resolved.Model, resolved.Provider)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, desc, err := ai.Ping(ctx, resolved)
		if err != nil {
			fmt.Println("✗")
			return err
		}
		fmt.Printf("✓ %s\n", desc)
		return nil
	case "use":
		if len(args) != 2 {
			return fmt.Errorf("usage: prreview model use <name>")
		}
		if _, ok := cfg.Models[args[1]]; !ok {
			return fmt.Errorf("unknown model profile %q — see: prreview model list", args[1])
		}
		cfg.DefaultModelName = args[1]
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("default model → %s\n", args[1])
		return nil
	case "rm", "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: prreview model rm <name>")
		}
		if _, ok := cfg.Models[args[1]]; !ok {
			return fmt.Errorf("unknown model profile %q", args[1])
		}
		delete(cfg.Models, args[1])
		if cfg.DefaultModelName == args[1] {
			cfg.DefaultModelName = ""
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown model subcommand: %s (list | use | rm, or no args for interactive)", args[0])
	}
}

func runConfig(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		p := tea.NewProgram(tui.NewConfigForm(cfg))
		final, err := p.Run()
		if err != nil {
			return err
		}
		if form, ok := final.(tui.ConfigModel); ok {
			saved, saveErr := form.Saved()
			if saveErr != nil {
				return saveErr
			}
			if saved {
				path, _ := config.Path()
				fmt.Printf("Saved to %s\n", path)
			} else {
				fmt.Println("Cancelled, nothing saved.")
			}
		}
		return nil
	}

	switch args[0] {
	case "show":
		path, _ := config.Path()
		fmt.Printf("config       : %s\n", path)
		fmt.Printf("provider     : %s\n", cfg.Provider)
		fmt.Printf("base_url     : %s\n", orDefault(cfg.BaseURL, "(provider default)"))
		fmt.Printf("api_key      : %s\n", config.MaskKey(cfg.APIKey))
		fmt.Printf("model        : %s\n", cfg.Model)
		fmt.Printf("default_model: %s\n", orDefault(cfg.DefaultModelName, "(none — using flat config)"))
		fmt.Printf("profiles     : %d (prreview model list)\n", len(cfg.Models))
		fmt.Printf("depth        : %s\n", cfg.Depth)
		fmt.Printf("effort       : %s\n", cfg.Effort)
		fmt.Printf("max_tokens   : %d\n", cfg.MaxTokens)
		return nil
	case "set":
		if len(args) != 3 {
			return fmt.Errorf("usage: prreview config set <key> <value>")
		}
		switch args[1] {
		case "api_key":
			cfg.APIKey = args[2]
		case "provider":
			if args[2] != "anthropic" && args[2] != "openai" {
				return fmt.Errorf("provider must be anthropic or openai")
			}
			cfg.Provider = args[2]
		case "base_url":
			cfg.BaseURL = args[2]
		case "model":
			cfg.Model = args[2]
		case "default_model":
			if _, ok := cfg.Models[args[2]]; !ok {
				return fmt.Errorf("unknown model profile %q — see: prreview model list", args[2])
			}
			cfg.DefaultModelName = args[2]
		case "depth":
			switch args[2] {
			case "fast", "standard", "max":
				cfg.Depth = args[2]
			default:
				return fmt.Errorf("depth must be fast, standard, or max")
			}
		case "effort":
			switch args[2] {
			case "low", "medium", "high", "xhigh", "max":
				cfg.Effort = args[2]
			default:
				return fmt.Errorf("effort must be low, medium, high, xhigh, or max")
			}
		case "max_tokens":
			n, err := strconv.Atoi(args[2])
			if err != nil || n <= 0 {
				return fmt.Errorf("max_tokens must be a positive integer")
			}
			cfg.MaxTokens = n
		default:
			return fmt.Errorf("unknown config key: %s", args[1])
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Println("Saved.")
		return nil
	default:
		return fmt.Errorf("unknown config subcommand: %s", args[0])
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
