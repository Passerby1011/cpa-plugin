package plugin

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostAuthEntry builds one host auth entry for the record-ownership test.
func hostAuthEntry(name, typ, provider string) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{Name: name, Type: typ, Provider: provider}
}
