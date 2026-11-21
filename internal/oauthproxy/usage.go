package oauthproxy

import "github.com/grokbuildx/gbx/internal/runtimelog"

type (
	UsageTotals      = runtimelog.UsageTotals
	UsageTracker     = runtimelog.UsageTracker
	UsageModelTotals = runtimelog.UsageModelTotals
)

func NewUsageTracker() *UsageTracker { return runtimelog.NewUsageTracker() }

func FormatUsageSummary(totals []UsageModelTotals) string {
	return runtimelog.FormatUsageSummary(totals)
}

func formatTokenCompact(tokens int64) string {
	return runtimelog.FormatTokenCompact(tokens)
}
