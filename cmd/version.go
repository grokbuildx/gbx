package cmd

import (
	"fmt"
	"strings"

	"github.com/grokbuildx/gbx/internal/grokbundle"
	"github.com/grokbuildx/gbx/internal/groklaunch"
	"github.com/spf13/cobra"
)

// Version is set dynamically via ldflags during build.
var Version = "dev"

// versionText reports gbx and the Grok Build it runs. Release builds embed an
// open-source Grok Build; other builds name the external binary they would use.
func versionText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gbx version %s\n", Version)
	if grokbundle.Available() {
		fmt.Fprintf(&b, "Grok Build %s\n", grokbundle.Describe())
		fmt.Fprintf(&b, "  source: %s/tree/%s\n", grokbundle.SourceRepository, grokbundle.Revision)
		return b.String()
	}
	b.WriteString("Grok Build not bundled in this build")
	if path, err := groklaunch.LookPath(); err == nil {
		fmt.Fprintf(&b, "; using %s", path)
	}
	b.WriteString("\n")
	return b.String()
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the gbx and bundled Grok Build versions",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Print(versionText())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)

	// Setting Version makes cobra handle --version itself. Without it the flag
	// is unknown, and because FParseErrWhitelist.UnknownFlags is enabled it
	// would be dropped silently and the root RunE would launch a session.
	rootCmd.Version = Version
	rootCmd.SetVersionTemplate("{{gbxVersionText}}")
	cobra.AddTemplateFunc("gbxVersionText", versionText)
}
