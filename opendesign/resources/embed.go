// Package resources holds this plugin's embedded panel page.
package resources

import (
	_ "embed"
	"strings"
)

//go:embed panel.html
var panelPage string

// managementBasePlaceholder is the token the page carries where the management
// base belongs, and PanelWithBase replaces it.
//
// The page is served from an UNAUTHENTICATED resource route, so it cannot ask the
// host for the management base at render time — and the base is host-configurable,
// so baking a literal "/v0/management" into the HTML would break on a host that
// mounts it elsewhere. One placeholder, filled in per request, keeps the page a
// static file while the base stays a runtime value.
const managementBasePlaceholder = "__MANAGEMENT_BASE__"

// Panel is the browser-facing page this plugin serves under
// /v0/resource/plugins/opendesign/panel, with the management base left as its
// placeholder.
func Panel() []byte { return []byte(panelPage) }

// PanelWithBase is Panel with the management base substituted in. base is the
// full management path prefix (for example "/v0/management/plugins/opendesign").
//
// A base that is empty, or that still carries the placeholder, leaves the page
// untouched rather than producing a page whose every request would 404: an
// unfetchable page is a better failure than a page claiming a route that is not
// there.
func PanelWithBase(base string) []byte {
	base = strings.TrimSpace(base)
	if base == "" || strings.Contains(base, managementBasePlaceholder) {
		return Panel()
	}
	return []byte(strings.ReplaceAll(panelPage, managementBasePlaceholder, base))
}
