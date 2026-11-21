package cmd

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/selfupdate"
	"github.com/spf13/cobra"
)

var updateMethod string

var updateCmd = func() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update gbx to the latest version",
		Long: `Update gbx to the latest release.

gbx detects how it was installed (npm, go install, or a downloaded binary)
and offers that method first. --method self|npm|go skips the prompt, which
is required when stdin is not a terminal. The self method downloads the
release archive for this platform, checks it against the release's
SHA256SUMS, and keeps the previous binary as a backup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(updateMethod)
		},
	}
	cmd.Flags().StringVar(&updateMethod, "method", "", "Update method: self, npm, or go (skips the prompt)")
	return cmd
}()

func runUpdate(method string) error {
	method = strings.ToLower(strings.TrimSpace(method))
	if method != "" && method != "self" && method != "npm" && method != "go" {
		return fmt.Errorf("unknown update method %q; use self, npm, or go", method)
	}
	fmt.Printf("Current version: %s\n", Version)
	fmt.Println("Checking for updates...")

	latestVersion, npmReady := selfupdate.LatestVersions()
	if latestVersion == "unknown" {
		fmt.Println("⚠️  Could not check for latest version: neither the GitHub release nor the npm registry answered")
	} else {
		fmt.Printf("Latest version: %s\n\n", latestVersion)
	}
	if Version != "dev" && latestVersion != "unknown" && selfupdate.CanonicalReleaseTag(Version) == selfupdate.CanonicalReleaseTag(latestVersion) && method != "npm" {
		fmt.Println("✨ You are already on the latest version!")
		return nil
	}
	// The npm package is published by a later job than the GitHub release, so a
	// brand-new version can exist for hours without an npm package. Offering npm
	// then installs an older version (or fails), and a launcher replaced by npm
	// has to be reinstalled by hand.

	detected := selfupdate.DetectInstallMethod()
	suggested := detected
	if detected == "npm" && !npmReady {
		// npm can only install its own latest, which is still the old release.
		// Replacing the binary in place leaves the npm package installed; on
		// Windows a running executable cannot be replaced, so there is nothing
		// to offer yet.
		if runtime.GOOS == "windows" && method == "" {
			return fmt.Errorf("npm has not published %s yet (the registry lags the GitHub release) and Windows cannot replace a running binary; retry later or run gbx update --method go", latestVersion)
		}
		suggested = "self"
	}
	switch method {
	case "":
		if !term.IsTerminal(os.Stdin.Fd()) {
			available := "self|go"
			if npmReady {
				available = "self|npm|go"
			}
			return fmt.Errorf("no terminal to ask on; run gbx update --method %s (suggested) or %s", suggested, available)
		}
		if detected == "npm" && !npmReady {
			fmt.Println(locale.T(
				"npm 上还没有这个版本；本次只替换二进制文件，不重装 npm 包。",
				"npm does not carry this version yet; replacing the binary in place instead of reinstalling the npm package."))
		}
		method = promptUpdateMethod(suggested, npmReady)
	case "self", "npm", "go":
	default:
		return fmt.Errorf("unknown update method %q; use self, npm, or go", method)
	}

	switch method {
	case "self":
		fmt.Println("Downloading latest gbx binary...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		backup, err := selfupdate.SelfUpdate(ctx, latestVersion)
		if err != nil {
			return fmt.Errorf("self-update failed (try the go method instead): %w", err)
		}
		if backup != "" {
			fmt.Printf("Previous version retained at: %s\n", backup)
		}
		fmt.Println("\n🎉 Successfully updated gbx to the latest version!")
	case "npm":
		if !npmReady {
			return fmt.Errorf("npm does not have %s yet (publishing lags the GitHub release); use --method self or go", latestVersion)
		}
		recovered, args, backup, err := selfupdate.UpdateViaNpm(latestVersion)
		if err != nil {
			if len(args) > 0 {
				return fmt.Errorf("npm update failed: %w; retry with npm %s", err, strings.Join(args, " "))
			}
			return err
		}
		if backup != "" {
			fmt.Printf("Previous version retained at: %s\n", backup)
		}
		if recovered {
			fmt.Printf("\nRecovered gbx %s with the release binary; npm installation did not complete.\n", selfupdate.CanonicalReleaseTag(latestVersion))
			fmt.Println("To repair the npm package later: npm " + strings.Join(args, " "))
		} else {
			fmt.Printf("\n🎉 Successfully updated gbx to %s via npm!\n", selfupdate.CanonicalReleaseTag(latestVersion))
		}
		return nil
	case "go":
		fmt.Println("Updating via Go... Running 'go install github.com/grokbuildx/gbx@latest'")
		if err := selfupdate.RunCommand("go", "install", "github.com/grokbuildx/gbx@latest"); err != nil {
			return fmt.Errorf("go update failed: %w", err)
		}
		fmt.Println("\n🎉 Successfully updated gbx to the latest version via Go!")
	case "manual":
		fmt.Println("\nPlease visit the following link to check alternative installation methods:")
		fmt.Println("🔗 https://github.com/grokbuildx/gbx#安装与编译")
	default:
		fmt.Println("Update cancelled.")
	}
	return nil
}

// updateChoice is one row of the update prompt; its number is its position.
type updateChoice struct{ method, text string }

// updateChoices lists the methods this run can actually use: npm is left out
// until the registry carries the version being installed.
func updateChoices(npmReady bool) []updateChoice {
	choices := []updateChoice{
		{"self", "Auto update (download latest binary)"},
	}
	if npmReady {
		choices = append(choices, updateChoice{"npm", "Update via npm (Global install)"})
	}
	return append(choices, updateChoice{"go", "Update via Go (go install)"})
}

// promptUpdateMethod asks which method to use; Enter picks the detected one.
func promptUpdateMethod(detected string, npmReady bool) string {
	choices := updateChoices(npmReady)
	labels := make(map[string]string, len(choices))
	fmt.Println(locale.T("选择更新方式:", "Choose update method:"))
	defaultChoice := choices[0].method
	for i, row := range choices {
		number := fmt.Sprintf("%d", i+1)
		labels[row.method] = number
		text := number + ". " + row.text
		if row.method == detected {
			text += "  ← detected"
			defaultChoice = detected
		}
		fmt.Println(text)
	}
	manual := len(choices) + 1
	cancel := len(choices) + 2
	fmt.Printf("%d. View installation instructions\n%d. Cancel\n", manual, cancel)
	fmt.Printf("Choose [1-%d] (Enter = %s): ", cancel, labels[defaultChoice])
	var choice string
	_, _ = fmt.Scanln(&choice)
	trimmed := strings.ToLower(strings.TrimSpace(choice))
	if trimmed == "" {
		return defaultChoice
	}
	for i, row := range choices {
		if trimmed == fmt.Sprintf("%d", i+1) || trimmed == row.method ||
			(row.method == "self" && trimmed == "auto") {
			return row.method
		}
	}
	switch trimmed {
	case fmt.Sprintf("%d", manual), "manual":
		return "manual"
	default:
		return "cancel"
	}
}

func init() {
	rootCmd.AddCommand(updateCmd)
}
