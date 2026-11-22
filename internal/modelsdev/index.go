package modelsdev

import (
	"fmt"
	"slices"
	"strings"
)

// Index retains reasoning, modality, and output-limit metadata and exact aliases
// from a catalog. It is immutable after construction and safe for concurrent lookups.
type Index struct {
	providers map[string]map[string]reasoningEntry
	aliases   map[string]reasoningEntry
}

type reasoningEntry struct {
	efforts             []string
	modalities          []string
	maxOutput           int
	priority            int
	thinking            bool
	hasEffort           bool
	hasBudget           bool
	ambiguous           bool
	thinkingAmbiguous   bool
	modeAmbiguous       bool
	modalitiesAmbiguous bool
	outputAmbiguous     bool
}

func newIndex(data []byte) (*Index, error) {
	providers, err := decodeAllProviders(data)
	if err != nil {
		return nil, err
	}
	return newIndexFromProviders(providers)
}

func newIndexFromProviders(providers map[string]Provider) (*Index, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("models.dev metadata is empty")
	}
	index := &Index{providers: make(map[string]map[string]reasoningEntry), aliases: make(map[string]reasoningEntry)}
	for providerID, provider := range providers {
		providerID = normalizeAlias(providerID)
		local := make(map[string]reasoningEntry)
		index.providers[providerID] = local
		for key, model := range provider.Models {
			// Metadata also serves existing routes/history. Deprecation in one
			// catalog does not withdraw a model from another provider's catalog.
			efforts := model.Efforts()
			modalities := model.InputModalities()
			maxOutput := model.Limit.Output
			thinking := model.Reasoning
			hasEffort := model.HasEffortOption()
			hasBudget := model.SupportsBudgetTokens()
			// Prefer the canonical model owner's entry. OpenCode's two
			// catalogs are references when the owner has no matching row.
			priority := 3
			owner, _, _ := strings.Cut(normalizeAlias(model.CanonicalID), "/")
			switch {
			case owner != "" && owner == providerID:
				priority = 0
			case providerID == "opencode-go":
				priority = 1
			case providerID == "opencode":
				priority = 2
			}
			for _, alias := range []string{key, model.ID, model.CanonicalID} {
				if strings.TrimSpace(alias) == "" {
					continue
				}
				addModelAlias(local, alias, efforts, modalities, maxOutput, thinking, hasEffort, hasBudget, 0)
				addModelAlias(index.aliases, alias, efforts, modalities, maxOutput, thinking, hasEffort, hasBudget, priority*2)
				addModelAlias(index.aliases, providerID+"/"+alias, efforts, modalities, maxOutput, thinking, hasEffort, hasBudget, priority*2)
			}
			if _, bare, ok := strings.Cut(model.CanonicalID, "/"); ok {
				addModelAlias(index.aliases, bare, efforts, modalities, maxOutput, thinking, hasEffort, hasBudget, priority*2)
			}
			addModelAlias(local, model.Name, efforts, modalities, maxOutput, thinking, hasEffort, hasBudget, 1)
			addModelAlias(index.aliases, model.Name, efforts, modalities, maxOutput, thinking, hasEffort, hasBudget, priority*2+1)
		}
	}
	if len(index.aliases) == 0 {
		return nil, fmt.Errorf("models.dev metadata has no models")
	}
	return index, nil
}

func addModelAlias(index map[string]reasoningEntry, alias string, efforts, modalities []string, maxOutput int, thinking, hasEffort, hasBudget bool, priority int) {
	alias = normalizeAlias(alias)
	if alias == "" {
		return
	}
	previous, exists := index[alias]
	if !exists || priority < previous.priority {
		index[alias] = reasoningEntry{
			efforts:    efforts,
			modalities: modalities,
			maxOutput:  maxOutput,
			thinking:   thinking,
			hasEffort:  hasEffort,
			hasBudget:  hasBudget,
			priority:   priority,
		}
		return
	}
	if priority == previous.priority {
		if !slices.Equal(previous.efforts, efforts) {
			previous.ambiguous = true
		}
		if previous.thinking != thinking {
			previous.thinkingAmbiguous = true
		}
		if previous.hasEffort != hasEffort || previous.hasBudget != hasBudget {
			previous.modeAmbiguous = true
		}
		if len(modalities) > 0 {
			if len(previous.modalities) == 0 {
				previous.modalities = modalities
			} else if !slices.Equal(previous.modalities, modalities) {
				previous.modalitiesAmbiguous = true
			}
		}
		if maxOutput > 0 {
			if previous.maxOutput == 0 {
				previous.maxOutput = maxOutput
			} else if previous.maxOutput != maxOutput {
				previous.outputAmbiguous = true
			}
		}
		index[alias] = previous
	}
}

// Efforts resolves an exact provider entry first, then a canonical/reference
// entry. Conflicting equal-priority aliases are not merged or guessed. Found
// with an empty slice means the model has no selectable reasoning tiers.
// The returned slice is a copy so callers cannot mutate the shared index.
func (index *Index) Efforts(providerID, modelID, displayName string) ([]string, bool) {
	if entry, ok := index.lookupWithPolicy(providerID, modelID, displayName, func(e reasoningEntry) bool { return !e.ambiguous }, true); ok {
		return slices.Clone(entry.efforts), true
	}
	return nil, false
}

// SupportsThinking reports whether models.dev unambiguously declares the model
// as reasoning-capable (including budget-only or toggle-only thinking models
// whose Efforts() slice is empty). Unknown or ambiguous models return (false, false).
func (index *Index) SupportsThinking(providerID, modelID, displayName string) (bool, bool) {
	if entry, ok := index.lookupWithPolicy(providerID, modelID, displayName, func(e reasoningEntry) bool { return !e.thinkingAmbiguous }, true); ok {
		return entry.thinking, true
	}
	return false, false
}

// ThinkingMode reports whether a reasoning-capable model is adaptive/effort-only
// (has effort tiers without budget_tokens) and whether it accepts budget_tokens.
func (index *Index) ThinkingMode(providerID, modelID, displayName string) (adaptive bool, supportsBudget bool, found bool) {
	if entry, ok := index.lookupWithPolicy(providerID, modelID, displayName, func(e reasoningEntry) bool {
		return !e.thinkingAmbiguous && !e.modeAmbiguous && e.thinking
	}, true); ok {
		return entry.hasEffort && !entry.hasBudget, entry.hasBudget, true
	}
	return false, false, false
}

// InputModalities resolves advertised input modalities (["text"] or ["text", "image"])
// for a model when models.dev unambiguously declares them. Unknown models return (nil, false).
func (index *Index) InputModalities(providerID, modelID, displayName string) ([]string, bool) {
	if entry, ok := index.lookupWithPolicy(providerID, modelID, displayName, func(e reasoningEntry) bool {
		return !e.modalitiesAmbiguous && len(e.modalities) > 0
	}, false); ok {
		return slices.Clone(entry.modalities), true
	}
	return nil, false
}

// MaxOutputTokens resolves the advertised maximum output token budget when known.
func (index *Index) MaxOutputTokens(providerID, modelID, displayName string) (int, bool) {
	if entry, ok := index.lookupWithPolicy(providerID, modelID, displayName, func(e reasoningEntry) bool {
		return !e.outputAmbiguous && e.maxOutput > 0
	}, false); ok {
		return entry.maxOutput, true
	}
	return 0, false
}

func (index *Index) lookup(providerID, modelID, displayName string, accept func(reasoningEntry) bool) (reasoningEntry, bool) {
	return index.lookupWithPolicy(providerID, modelID, displayName, accept, false)
}

func (index *Index) lookupWithPolicy(providerID, modelID, displayName string, accept func(reasoningEntry) bool, rejectThinkingDowngrade bool) (reasoningEntry, bool) {
	if index == nil {
		return reasoningEntry{}, false
	}
	local := index.providers[normalizeAlias(providerID)]
	exact := []string{normalizeAlias(modelID), normalizeAlias(displayName)}
	if entry, matched, rejected := lookupAliases(local, index.aliases, exact, accept); matched {
		return entry, true
	} else if rejected {
		return reasoningEntry{}, false
	}
	secondary := secondaryAliases(exact)
	if len(secondary) == 0 {
		return reasoningEntry{}, false
	}
	secondaryAccept := accept
	if rejectThinkingDowngrade && aliasesCarryThinkingQualifier(exact) {
		secondaryAccept = func(e reasoningEntry) bool {
			return e.thinking && accept(e)
		}
	}
	entry, matched, _ := lookupAliases(local, index.aliases, secondary, secondaryAccept)
	return entry, matched
}

func aliasesCarryThinkingQualifier(aliases []string) bool {
	for _, a := range aliases {
		if aliasHasThinkingQualifier(a) {
			return true
		}
	}
	return false
}

func aliasHasThinkingQualifier(alias string) bool {
	if alias == "" {
		return false
	}
	cur := alias
	for {
		if strings.HasSuffix(cur, ":thinking") || strings.HasSuffix(cur, "-thinking") {
			return true
		}
		next := stripSingleGatewayColonTag(cur)
		if next == cur {
			return false
		}
		cur = next
	}
}

func lookupAliases(local, global map[string]reasoningEntry, aliases []string, accept func(reasoningEntry) bool) (reasoningEntry, bool, bool) {
	rejected := false
	if local != nil {
		for _, alias := range aliases {
			if alias == "" {
				continue
			}
			if entry, ok := local[alias]; ok {
				if accept(entry) {
					return entry, true, false
				}
				rejected = true
			}
		}
	}
	for _, alias := range aliases {
		if alias == "" {
			continue
		}
		if entry, ok := global[alias]; ok {
			if accept(entry) {
				return entry, true, false
			}
			rejected = true
		}
	}
	return reasoningEntry{}, false, rejected
}

// secondaryAliases generates lossless gateway-formatting variants for exact
// misses without fuzzy version stripping: colon routing qualifiers (:free,
// :thinking, chained :free:nitro, etc.), -thinking suffixes, unknown vendor/
// prefixes, and digit separator equivalence (4.6 <-> 4-6).
func secondaryAliases(exact []string) []string {
	seen := make(map[string]bool, len(exact)*4)
	for _, a := range exact {
		if a != "" {
			seen[a] = true
		}
	}
	var out []string
	add := func(candidate string) {
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	addWithSeparators := func(candidate string) {
		add(candidate)
		if swapped := swapDigitSeparator(candidate, '.', '-'); swapped != candidate {
			add(swapped)
		}
		if swapped := swapDigitSeparator(candidate, '-', '.'); swapped != candidate {
			add(swapped)
		}
	}
	for _, base := range exact {
		if base == "" {
			continue
		}
		candidates := []string{base}
		cur := base
		for {
			next := stripSingleGatewayColonTag(cur)
			if next == cur {
				break
			}
			candidates = append(candidates, next)
			cur = next
		}
		for _, c := range candidates {
			addWithSeparators(c)
			if noThinking := strings.TrimSuffix(c, "-thinking"); noThinking != c && noThinking != "" {
				addWithSeparators(noThinking)
			}
			if slash := strings.LastIndexByte(c, '/'); slash > 0 && slash < len(c)-1 {
				tail := c[slash+1:]
				addWithSeparators(tail)
				if noThinking := strings.TrimSuffix(tail, "-thinking"); noThinking != tail && noThinking != "" {
					addWithSeparators(noThinking)
				}
			}
		}
	}
	return out
}

func stripGatewayColonTag(alias string) string {
	cur := alias
	for {
		next := stripSingleGatewayColonTag(cur)
		if next == cur {
			return cur
		}
		cur = next
	}
}

func stripSingleGatewayColonTag(alias string) string {
	idx := strings.LastIndexByte(alias, ':')
	if idx <= 0 || idx == len(alias)-1 {
		return alias
	}
	tag := alias[idx+1:]
	// Only strip alpha-leading routing qualifiers such as :free, :thinking,
	// :exacto, :nitro, :beta, :online, :extended.
	if tag[0] < 'a' || tag[0] > 'z' {
		return alias
	}
	for i := 1; i < len(tag); i++ {
		c := tag[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return alias
		}
	}
	return alias[:idx]
}

func swapDigitSeparator(alias string, from, to byte) string {
	if !strings.ContainsRune(alias, rune(from)) {
		return alias
	}
	buf := []byte(alias)
	changed := false
	for i := 1; i < len(buf)-1; i++ {
		if buf[i] == from && buf[i-1] >= '0' && buf[i-1] <= '9' && buf[i+1] >= '0' && buf[i+1] <= '9' {
			buf[i] = to
			changed = true
		}
	}
	if !changed {
		return alias
	}
	return string(buf)
}

func normalizeAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	alias = strings.TrimSuffix(alias, "[1m]")
	if strings.ContainsAny(alias, " \t\n\r\v\f") {
		alias = strings.Join(strings.Fields(alias), "-")
	}
	return alias
}
