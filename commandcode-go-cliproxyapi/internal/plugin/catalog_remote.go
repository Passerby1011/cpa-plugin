package plugin

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// remoteCatalogTimeout bounds one fetch of the remote model list. These URLs
// are plain static files on a CDN, so a slow response means something is wrong
// and the caller should fall back rather than block a refresh.
const remoteCatalogTimeout = 20 * time.Second

// remoteModelIDRe extracts model ids from a fetched source file.
//
// The reference proxy keeps its model table as JS object literals
// (`{ id: 'claude-sonnet-4-6', name: '...' }`), so the id is matched directly.
// A JSON-shaped source (`"id": "..."`) is accepted too, because the same file
// may be re-emitted later and a source that switches shape should keep working.
//
// Deliberately permissive about WHERE the id appears but strict about what an
// id looks like: a bare word like `name` or `id` is not a model, so the pattern
// requires an actual model-ish token (a vendor prefix or a known family).
var remoteModelIDRe = regexp.MustCompile(`(?i)\bid\s*:\s*['"]([^'"]{2,80})['"]|"id"\s*:\s*"([^"]{2,80})"`)

// remoteCatalogCache memoises the last successful fetch so a polling refresh
// does not re-download the same file, and so a transient failure can fall back
// to the previous good list instead of blanking the catalog.
type remoteCatalogCache struct {
	mu        sync.Mutex
	url       string
	ids       []string
	fetchedAt time.Time
}

// fetchRemoteCatalog returns the model ids advertised by cfg's remote source.
//
// On a fetch or parse failure the LAST GOOD result is returned when one exists
// for the same URL, so a CDN blip cannot empty a working catalog. When there is
// nothing cached the error is returned and the caller falls through to the
// static list.
func (m *Manager) fetchRemoteCatalog(ctx context.Context, url string, minInterval time.Duration) ([]string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, fmt.Errorf("no remote catalog url configured")
	}
	if m.bridge == nil {
		return nil, fmt.Errorf("remote catalog unavailable: no host bridge")
	}

	m.remoteMu.Lock()
	if m.remote != nil && m.remote.url == url && len(m.remote.ids) > 0 {
		if minInterval > 0 && time.Since(m.remote.fetchedAt) < minInterval {
			ids := append([]string(nil), m.remote.ids...)
			m.remoteMu.Unlock()
			return ids, nil
		}
	}
	m.remoteMu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, remoteCatalogTimeout)
	defer cancel()

	resp, errDo := m.bridge.Do(fetchCtx, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    url,
		Headers: http.Header{
			"Accept":     []string{"text/plain, application/json"},
			"User-Agent": []string{"commandcode-go-cliproxyapi"},
		},
	})
	if errDo != nil {
		return m.cachedRemote(url), fmt.Errorf("remote catalog fetch failed: %w", errDo)
	}
	if resp.StatusCode != http.StatusOK {
		return m.cachedRemote(url), fmt.Errorf("remote catalog fetch rejected (%d)", resp.StatusCode)
	}

	ids := parseRemoteModelIDs(string(resp.Body))
	if len(ids) == 0 {
		return m.cachedRemote(url), fmt.Errorf("remote catalog contained no model ids")
	}

	m.remoteMu.Lock()
	m.remote = &remoteCatalogCache{url: url, ids: append([]string(nil), ids...), fetchedAt: time.Now()}
	m.remoteMu.Unlock()

	if m.bridge != nil {
		_ = m.bridge.Log("info", "remote catalog synchronized", map[string]any{
			"models": len(ids),
			"source": redactCatalogURL(url),
		})
	}
	return ids, nil
}

// cachedRemote returns the previously fetched list for this URL, or nil.
// Used on failure so a working catalog survives a transient outage.
func (m *Manager) cachedRemote(url string) []string {
	m.remoteMu.Lock()
	defer m.remoteMu.Unlock()
	if m.remote != nil && m.remote.url == url && len(m.remote.ids) > 0 {
		return append([]string(nil), m.remote.ids...)
	}
	return nil
}

// parseRemoteModelIDs pulls model ids out of a fetched source file, deduped and
// sorted. Extracted here (rather than inline) because it is the one part of
// this feature that can be tested without a network.
func parseRemoteModelIDs(body string) []string {
	seen := make(map[string]struct{})
	for _, m := range remoteModelIDRe.FindAllStringSubmatch(body, -1) {
		id := strings.TrimSpace(m[1])
		if id == "" {
			id = strings.TrimSpace(m[2])
		}
		if id == "" || !looksLikeModelID(id) {
			continue
		}
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// looksLikeModelID filters prose and field names out of the extraction.
//
// Without it a source file's own structure words ("id", "name", "type") would
// be published as models. An id qualifies when it carries a vendor prefix
// (anything with a slash) or starts with a known model family.
func looksLikeModelID(id string) bool {
	if strings.ContainsAny(id, " \t") {
		return false
	}
	if strings.Contains(id, "/") {
		return true
	}
	lower := strings.ToLower(id)
	for _, prefix := range []string{
		"claude-", "gpt-", "o1-", "o3-", "o4-", "gemini-", "deepseek-",
		"kimi-", "glm-", "minimax-", "qwen", "step-", "mimo-", "grok-",
		"commandcode-", "llama-", "mistral-",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// redactCatalogURL strips any query string or userinfo before a URL reaches a
// log line: a source URL can carry a token in the query.
func redactCatalogURL(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			return raw[:i+3] + rest[at+1:]
		}
	}
	return raw
}
