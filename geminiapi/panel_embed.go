// panel_embed.go embeds the management page into the .so.
//
// The page lives at the module root and is embedded HERE (package main) because
// go:embed cannot reach a parent directory: internal/plugin would have to hold
// its own copy and the two would drift. main passes the bytes to the plugin
// package at init.
//
// Editing panel.html therefore requires a rebuild — gofmt and vet never look
// inside it.
package main

import (
	_ "embed"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/plugin"
)

//go:embed panel.html
var panelHTML []byte

func init() {
	plugin.SetPanel(panelHTML)
}
