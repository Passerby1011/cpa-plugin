// Package resources holds this plugin's embedded panel page.
package resources

import _ "embed"

//go:embed panel.html
var panelPage string

// Panel is the browser-facing page this plugin serves under
// /v0/resource/plugins/opendesign/.
func Panel() []byte { return []byte(panelPage) }
