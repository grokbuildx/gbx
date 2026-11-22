package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "gbx",
	Short: "Multi-provider launcher for Grok Build and Codex",
	Long: `gbx launches Grok Build with the active provider from ~/.gbx/config.yaml.

Common commands:
  gbx                         Start Grok Build with the active provider
  gbx --provider <name>       Launch once with another provider
  gbx use [name]              Switch providers; default uses Grok Build's own login
  gbx set [name]              Add or update a provider
  gbx ls                      List providers
  gbx preview [name]          Preview the Grok Build settings JSON
  gbx oauth <backend>         Log in with a subscription account
  gbx codex run [-p <name>]   Connect models and open ChatGPT / Codex (auto-stop on quit)
  gbx serve -p <name>         Use Grok Build in a local browser
  gbx doctor                  Check environment and provider connectivity
  gbx cloud login|push|pull   Manage encrypted configuration sync
  gbx log on|off              Configure per-session logs

Grok arguments pass through. Use gbx grok to bypass gbx command names
(for example, gbx grok doctor and gbx grok update).
`,
	// Execute prints every error once; cobra printing it as well duplicated it.
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Arguments are valid by now. A launch failure gets its own message.
		cmd.SilenceUsage = true
		return runGrok(args, "")
	},
}

func Execute() {
	configureLogging()
	// gbx codex start/stop recover under their own lifecycle lock.
	if len(os.Args) < 2 || os.Args[1] != "codex" {
		recoverStaleProxy()
	}

	if len(os.Args) > 1 {
		name, args, dispatch, err := resolveRootGrokArgs(os.Args[1:])
		if dispatch {
			if err == nil {
				err = runGrokWithOverride(args, name)
			}
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					os.Exit(exitErr.ExitCode())
				}
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}

	rootCmd.FParseErrWhitelist.UnknownFlags = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitCoder interface{ ExitCode() int }
		if errors.As(err, &exitCoder) {
			os.Exit(exitCoder.ExitCode())
		}
		os.Exit(1)
	}
}

// configureLogging makes the persisted threshold available without opening a
// shared file. A Grok session or temporary provider runtime opens its own.
func configureLogging() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	oauthproxy.ConfigureLogLevel(configuredLogLevel(cfg.LogLevel))
}

func isGbxCommand(arg string) bool {
	switch arg {
	// --version asks about gbx. -v is forwarded to Grok.
	case "help", "completion", "-h", "--help", "--version":
		return true
	}

	for _, command := range rootCmd.Commands() {
		if command.Name() == arg {
			return true
		}
		for _, alias := range command.Aliases {
			if alias == arg {
				return true
			}
		}
	}

	return false
}

func resolveRootGrokArgs(rawArgs []string) (name string, rest []string, dispatch bool, err error) {
	if len(rawArgs) == 0 {
		return "", nil, false, nil
	}
	name, rest, handled, delimited, err := parseLaunchOptionsWithDelim(rawArgs)
	first := rawArgs[0]
	if !handled && isGbxCommand(first) {
		return "", nil, false, nil
	}
	if err != nil {
		return name, nil, true, err
	}
	if !delimited && len(rest) > 0 && rest[0] == "grok" {
		rest = rest[1:]
	}
	return name, rest, true, nil
}

// Only a leading --provider selects a GBX provider. Grok flags such as
// --agent are passed through. -- separates GBX's option from Grok arguments.
func parseLaunchOptions(args []string) (name string, rest []string, handled bool, err error) {
	name, rest, handled, _, err = parseLaunchOptionsWithDelim(args)
	return name, rest, handled, err
}

func parseLaunchOptionsWithDelim(args []string) (name string, rest []string, handled bool, delimited bool, err error) {
	if len(args) == 0 {
		return "", args, false, false, nil
	}
	key, value, inline := strings.Cut(args[0], "=")
	if key != "--provider" {
		return "", args, false, false, nil
	}
	rest = args[1:]
	if !inline && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		value, rest = rest[0], rest[1:]
	}
	name = strings.TrimSpace(value)
	if name == "" {
		return "", nil, true, false, fmt.Errorf("--provider requires a name")
	}
	if len(rest) > 0 {
		key, _, _ := strings.Cut(rest[0], "=")
		switch key {
		case "--provider":
			return "", nil, true, false, fmt.Errorf("--provider may be specified only once")
		case "--":
			delimited = true
			rest = rest[1:]
		}
	}
	return name, rest, true, delimited, nil
}

func runGrok(args []string, name string) error {
	return runGrokWithOverride(args, name)
}

func runGrokWithOverride(args []string, override string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load gbx config: %w", err)
	}
	explicitProvider := strings.TrimSpace(override) != ""
	name := override
	if !explicitProvider {
		name = cfg.ActiveProvider
	}
	return runGrokAgentWithOptions(cfg, name, explicitProvider, args)
}

// resolveProvider returns the active configured provider.
func resolveProvider() (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, fmt.Errorf("failed to load config: %w", err)
	}
	return resolveProviderFromConfig(cfg, cfg.ActiveProvider)
}

func resolveProviderFromConfig(cfg *provider.Config, name string) (provider.Provider, error) {
	if name == "" {
		return provider.Provider{}, errors.New(locale.T(
			"未选择 Provider。用 gbx set 添加、gbx use <name> 选择，或用 gbx use default 直接以 Grok Build 自身登录启动",
			"no provider selected. Add one with gbx set, choose one with gbx use <name>, or run Grok Build on its own login with gbx use default",
		))
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return provider.Provider{}, fmt.Errorf("provider %q not found in configuration", name)
	}
	p.Name = name
	return p, nil
}
