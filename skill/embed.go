// Package skill embeds the canonical ecu-computer-use skill files so the
// control plane can serve a preconfigured copy for download.
package skill

import "embed"

// Files holds the ecu-computer-use skill tree. The go:embed pattern below
// excludes entries whose names start with "_" or "." (so __pycache__ is skipped
// automatically); test files are filtered out at zip-build time.
//
//go:embed ecu-computer-use
var Files embed.FS
