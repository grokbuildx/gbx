package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/spf13/cobra"
)

var lsCmd = newProviderListCommand("ls")

func newProviderListCommand(use string) *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "List registered providers",
		Long: `List providers from ~/.gbx/config.yaml. The USED BY column shows which
entry is selected for Grok Build or Codex. The default row uses Grok Build's own login.

-a/--all switches the table to a detailed view with full model pools.

Examples:
  gbx ls
  gbx ls -a
  gbx provider ls
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			return printProviders(cmd.OutOrStdout(), cfg, showAll, "No providers added yet. Use 'gbx set' to add one.", "Registered providers:")
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false, "Detailed view with full model pools")
	return cmd
}

func printProviders(out io.Writer, cfg *provider.Config, showAll bool, emptyMessage, heading string) error {
	var names []string
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		defer fmt.Fprintln(out, "\n"+emptyMessage)
	}

	fmt.Fprintln(out, heading)
	proxyNames := runningProxyProviders(context.Background())
	if showAll {
		fmt.Fprintln(out, provider.DefaultProvider)
		fmt.Fprintf(out, "    Used By  : %s\n", providerUsageSummary(cfg, provider.DefaultProvider, proxyNames...))
		description := defaultProviderDescription()
		fmt.Fprintf(out, "    Type     : %s\n", description)
		if len(names) > 0 {
			fmt.Fprintln(out)
		}
		return printProviderDetails(out, cfg, names, proxyNames)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tUSED BY\tTYPE/AUTH\tMODELS")
	defaultType := "grok-build/own-config"
	fmt.Fprintf(tw, "%s\t%s\t%s\t-\n", provider.DefaultProvider,
		providerUsageSummary(cfg, provider.DefaultProvider, proxyNames...), defaultType)
	for _, name := range names {
		p := cfg.Providers[name]
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\n",
			name,
			providerUsageSummary(cfg, name, proxyNames...),
			providerTypeAuthSummary(p),
			formatModelCount(p.Model),
		)
	}

	return tw.Flush()
}

func printProviderDetails(out io.Writer, cfg *provider.Config, names []string, proxyNames []string) error {
	for i, name := range names {
		p := cfg.Providers[name]
		fmt.Fprintln(out, name)
		fmt.Fprintf(out, "    Used By  : %s\n", providerUsageSummary(cfg, name, proxyNames...))
		fmt.Fprintf(out, "    Type     : %s\n", provider.ProtocolLabelForProvider(p))
		fmt.Fprintf(out, "    Auth     : %s\n", providerAuthLabel(p))
		if p.OAuthProvider != "" {
			fmt.Fprintf(out, "    OAuth    : %s\n", p.OAuthProvider)
		}
		fmt.Fprintf(out, "    Endpoint : %s\n", p.Endpoint)
		fmt.Fprintf(out, "    Default  : %s\n", providerDefaultModelSummary(p))
		fmt.Fprintf(out, "    Effort   : %s\n", providerEffortSummary(p))
		fmt.Fprintf(out, "    Fast     : %s\n", providerFastSummary(p))
		fmt.Fprintf(out, "    Context  : %s\n", providerOneMSummary(p))
		fmt.Fprintf(out, "    Models   : %s\n", formatModelCount(p.Model))
		if p.Model != "" {
			fmt.Fprintf(out, "    Pool IDs : %s\n", p.Model)
		}
		if i < len(names)-1 {
			fmt.Fprintln(out)
		}
	}
	return nil
}

// providerUsageSummary names the consumers of a provider. proxyNames lists all
// providers a running standalone proxy serves (primary and --with-provider).
func providerUsageSummary(cfg *provider.Config, name string, proxyNames ...string) string {
	usedBy := make([]string, 0, 3)
	if name == cfg.ActiveProvider {
		usedBy = append(usedBy, "Grok Build")
	}
	if name != "" && slices.Contains(proxyNames, name) {
		usedBy = append(usedBy, "Codex")
	}
	if len(usedBy) == 0 {
		return "-"
	}
	return strings.Join(usedBy, "+")
}

func formatModelCount(modelStr string) string {
	count := len(parseModelList(modelStr))
	if count == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", count)
}

func providerTypeAuthSummary(p provider.Provider) string {
	if oauthProvider := strings.ToLower(strings.TrimSpace(p.OAuthProvider)); oauthProvider != "" {
		return oauthProvider + "/oauth"
	}
	protocol := strings.ReplaceAll(provider.ProtocolLabelForProvider(p), " / ", "/")
	return protocol + "/" + providerAuthLabel(p)
}

func init() {
	rootCmd.AddCommand(lsCmd)
}

func defaultProviderDescription() string {
	return locale.T(
		"Grok Build 自身登录和设置",
		"Grok Build's own login and settings",
	)
}
