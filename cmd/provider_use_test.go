package cmd

import (
	"strings"
	"testing"

	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/provider"
)

// TestUseWithoutArgsAndNoProviders verifies bare `gbx use` fails with a
// guidance error before any TUI starts when nothing is configured.
func TestUseWithoutArgsAndNoProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := executeCommand(RootCmd(), "use")
	if err == nil {
		t.Fatalf("expected error, got nil (output: %q)", out)
	}
	// Both localized variants of the guidance message mention 'gbx set', so
	// the assertion stays locale-independent.
	if want := "'gbx set'"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestUseWithExplicitNameSwitchesActiveProvider keeps the non-interactive
// `gbx use <name>` path working, including config persistence.
func TestUseWithExplicitNameSwitchesActiveProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["alpha"] = provider.Provider{Name: "alpha", Type: "openai"}
	cfg.Providers["beta"] = provider.Provider{Name: "beta", Type: "openai_responses"}
	cfg.ActiveProvider = "alpha"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	out, err := executeCommand(RootCmd(), "use", "beta")
	if err != nil {
		t.Fatalf("unexpected error: %v (output: %q)", err, out)
	}

	// The success notice goes to the process stdout (fmt.Printf), not the
	// cobra output buffer, so assert on the persisted config instead.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveProvider != "beta" {
		t.Fatalf("persisted active provider = %q, want beta", reloaded.ActiveProvider)
	}
}

// TestUseUnknownProviderStillErrors preserves the explicit-not-found path.
func TestUseUnknownProviderStillErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	out, err := executeCommand(RootCmd(), "use", "missing")
	if err == nil {
		t.Fatalf("expected error, got nil (output: %q)", out)
	}
	for _, want := range []string{`"missing"`, "'gbx set'"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}
