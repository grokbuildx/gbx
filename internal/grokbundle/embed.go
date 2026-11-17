//go:build grokbundle

package grokbundle

import _ "embed"

// grok.gz is written by scripts/build-release.sh from scripts/build-grok.sh.
//
//go:embed grok.gz
var embeddedArchive []byte
