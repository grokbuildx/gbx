package selfupdate

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchLatestReleaseVersionFallsBackToWebRedirectOnRateLimit(t *testing.T) {
	var apiHits, webHits atomic.Int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer apiSrv.Close()

	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webHits.Add(1)
		w.Header().Set("Location", "https://github.com/grokbuildx/gbx/releases/tag/v1.0.0-2")
		w.WriteHeader(http.StatusFound)
	}))
	defer webSrv.Close()

	prevAPI, prevWeb, prevClient := LatestReleaseAPI, LatestReleaseWebURL, HTTPClient
	t.Cleanup(func() { LatestReleaseAPI, LatestReleaseWebURL, HTTPClient = prevAPI, prevWeb, prevClient })

	// Keep LatestReleaseAPI at default URL while routing requests to local fixtures.
	LatestReleaseAPI = defaultLatestReleaseAPI
	LatestReleaseWebURL = defaultLatestReleaseWebURL
	HTTPClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			switch req.URL.Host {
			case "api.github.com":
				cloned.URL.Scheme = "http"
				cloned.URL.Host = strings.TrimPrefix(apiSrv.URL, "http://")
			case "github.com":
				cloned.URL.Scheme = "http"
				cloned.URL.Host = strings.TrimPrefix(webSrv.URL, "http://")
			}
			return http.DefaultTransport.RoundTrip(cloned)
		}),
	}

	got, err := FetchLatestReleaseVersion()
	if err != nil {
		t.Fatalf("FetchLatestReleaseVersion() error = %v", err)
	}
	if got != "v1.0.0-2" {
		t.Fatalf("FetchLatestReleaseVersion() = %q, want v1.0.0-2", got)
	}
	if apiHits.Load() != 1 || webHits.Load() != 1 {
		t.Fatalf("hits api=%d web=%d, want 1 and 1", apiHits.Load(), webHits.Load())
	}
}

func TestParseConfiguredNpmRegistry(t *testing.T) {
	cases := []struct {
		name               string
		envUpper, envLower string
		npmrc              string
		want               string
	}{
		{
			name:     "env overrides npmrc",
			envUpper: "https://mirror.example.com/",
			npmrc:    "registry=https://other.example.com/\n",
			want:     "https://mirror.example.com",
		},
		{
			name:  "scoped registry wins over generic registry",
			npmrc: "# comment\nregistry=https://registry.npmmirror.com/\n@grokbuildx:registry=https://scoped.example.com/\n",
			want:  "https://scoped.example.com",
		},
		{
			name:  "generic registry from npmrc",
			npmrc: "; comment\nregistry = \"https://registry.npmmirror.com/\"\n",
			want:  "https://registry.npmmirror.com",
		},
		{
			name:  "rejects non-http schemes",
			npmrc: "registry=file:///etc/passwd\n",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseConfiguredNpmRegistry(tc.envUpper, tc.envLower, tc.npmrc); got != tc.want {
				t.Fatalf("parseConfiguredNpmRegistry() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchLatestNpmVersionUsesConfiguredMirrorFirst(t *testing.T) {
	var mirrorHits, defaultHits atomic.Int32
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"1.0.0-2"}`))
	}))
	defer mirrorSrv.Close()

	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer defaultSrv.Close()

	prevBase, prevClient := NpmRegistryBase, HTTPClient
	t.Cleanup(func() { NpmRegistryBase, HTTPClient = prevBase, prevClient })
	NpmRegistryBase = defaultSrv.URL
	HTTPClient = &http.Client{Timeout: 5 * time.Second}
	t.Setenv("NPM_CONFIG_REGISTRY", mirrorSrv.URL)
	t.Setenv("npm_config_registry", "")

	got, err := FetchLatestNpmVersion()
	if err != nil || got != "v1.0.0-2" {
		t.Fatalf("FetchLatestNpmVersion() = (%q, %v), want (v1.0.0-2, nil)", got, err)
	}
	if mirrorHits.Load() != 1 || defaultHits.Load() != 0 {
		t.Fatalf("mirrorHits=%d defaultHits=%d, want 1 and 0", mirrorHits.Load(), defaultHits.Load())
	}
}

func TestNpmUpdateArgsAndVerifyWithUnknownVersion(t *testing.T) {
	want := []string{"install", "-g", "@grokbuildx/gbx@latest", "--include=optional", "--prefer-online"}
	for _, ver := range []string{"", "unknown"} {
		if got := NpmUpdateArgs(ver); !reflect.DeepEqual(got, want) {
			t.Fatalf("NpmUpdateArgs(%q) = %v, want %v", ver, got, want)
		}
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	bin := filepath.Join(dir, "gbx.exe")
	if err := os.WriteFile(src, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"gbx version v1.0.0-2\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	if err := VerifyNpmInstall(bin, "unknown"); err != nil {
		t.Fatalf("VerifyNpmInstall(unknown) failed: %v", err)
	}
	if err := VerifyNpmInstall(bin, "v1.0.0-2"); err != nil {
		t.Fatalf("VerifyNpmInstall(v1.0.0-2) failed: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
