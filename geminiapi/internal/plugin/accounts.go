package plugin

// Account import: hand a credential to the BACKEND THAT OWNS IT, and report
// whether it can serve requests.
//
// # Two backends, two mechanisms — and neither is optional
//
// AIStudio2API and gemini-web2api-go do not share an account API, so "import an
// account" means something different per namespace:
//
//   - gemini-web2api-go owns a Google Cookie. A pasted Cookie header is POSTed
//     to its /admin/api/cookies, stored in its own database, then probed to
//     confirm it signs in.
//   - AIStudio2API never accepts a pasted Cookie. It signs accounts in with its
//     own browser and keeps the auth state itself, so the plugin starts that
//     sign-in (POST /api/accounts) and reports the task.
//
// Posting a Cookie at AI Studio would fail at a nonexistent endpoint, and
// pretending it worked is worse than saying so.
//
// # The credential never becomes this plugin's data
//
// A pasted cookie is a live Google session. This code therefore:
//
//   - sends it straight to the local backend's own admin surface over loopback,
//     and nowhere else;
//   - never logs it, never includes it in an error string, never puts it in a
//     management response, and never writes it to the plugin's own files;
//   - on a failed probe, deletes the account it just added so a dead cookie
//     does not linger in the pool.
//
// The operator-facing errors speak about WHICH COOKIES are missing and how to
// copy the right thing, because the failure mode is nearly always "copied one
// cookie from the Application panel instead of the whole Cookie header".

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// requiredCookies are the cookies a gemini.google.com request carries. Without
// them the backend serves the request anonymously or the account check fails.
var requiredCookies = []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSID"}

// cookieDomain matches the domains whose cookies the browser sends to
// gemini.google.com.
var cookieDomain = regexp.MustCompile(`^\.?(gemini\.)?google\.com$`)

// cookieName is the RFC 6265 token grammar, which is what a cookie header
// accepts; a value with anything else is a pasted header fragment, not a pair.
var cookieName = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)

// authuserPattern accepts the /u/N account index. Empty means 0.
var authuserPattern = regexp.MustCompile(`^[0-9]$`)

// importRequest is the management POST body.
type importRequest struct {
	// Namespace selects which backend receives the account. It is required:
	// the two backends take different inputs.
	Namespace string `json:"namespace"`
	// Cookie is the pasted "cookie" header value (with or without the
	// "Cookie:" prefix), or a JSON export of the same pairs. Used by
	// gemini-web only.
	Cookie string `json:"cookie"`
	// AuthUser is the /u/N index of the window the cookie came from.
	AuthUser string `json:"authuser,omitempty"`
	// Label names the account in the backend's pool.
	Label string `json:"label,omitempty"`
	// SkipProbe adds the account without checking it can sign in. It exists
	// for an offline deployment whose backend cannot reach Google right now;
	// the UI does not expose it by default.
	SkipProbe bool `json:"skip_probe,omitempty"`
	// LoginTimeout bounds AI Studio's browser sign-in, in seconds. Zero uses
	// the default.
	LoginTimeout int `json:"login_timeout,omitempty"`
}

// importAccountResponse is what the page receives. It never contains a cookie
// or the backend's keys — only whether the import worked and why not.
type importAccountResponse struct {
	OK        bool   `json:"ok"`
	Namespace string `json:"namespace"`
	// AccountID is the backend's own account id, useful for a later delete.
	AccountID int `json:"account_id,omitempty"`
	// Detail is the backend's own verdict (already free of cookie material).
	Detail string `json:"detail,omitempty"`
	// TaskID / TaskState report AI Studio's asynchronous sign-in.
	TaskID    string `json:"task_id,omitempty"`
	TaskState string `json:"task_state,omitempty"`
	// Error explains a failure in operator terms.
	Error string `json:"error,omitempty"`
}

// studioLoginDefaultTimeout bounds an AI Studio browser sign-in. It is generous
// because a human may have to complete a Google flow in the window the backend
// opens.
const studioLoginDefaultTimeout = 5 * time.Minute

// importTimeout covers gemini-web's own account check, which visits Google
// through the backend. A short timeout would report a false failure on a slow
// proxy.
const importTimeout = 90 * time.Second

// ParseCookieHeader turns pasted text into a normalized Cookie header value,
// keeping only the cookies that matter for a Google sign-in.
//
// Accepted shapes:
//   - the raw Cookie request-header value: "SID=...; HSID=...; ..."
//   - the same with a leading "cookie:" / "Cookie:" label (what DevTools shows)
//   - a JSON export: [{"name":"SID","value":"...","domain":"..."}, ...]
//   - HTML table text pasted from the Application panel (name, value, domain,
//     ... separated by tabs); only google.com rows are kept.
//
// It returns an operator-facing error naming the cookies that are missing.
func ParseCookieHeader(input string) (string, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return "", fmt.Errorf("没有收到 Cookie：请粘贴 gemini.google.com 请求头里 cookie 那一行的值")
	}
	// A "cookie:" label in front of the value is what DevTools' copy shows.
	lower := strings.ToLower(text)
	for _, prefix := range []string{"cookie:", "cookie "} {
		if strings.HasPrefix(lower, prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			lower = strings.ToLower(text)
			break
		}
	}

	pairs := map[string]string{}
	order := []string{}
	put := func(name, value string) {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" || !cookieName.MatchString(name) {
			return
		}
		if _, seen := pairs[name]; !seen {
			order = append(order, name)
			pairs[name] = value
		}
	}

	// JSON export first: it is unambiguous.
	if strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{") {
		var rows []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal([]byte(text), &rows); err == nil && len(rows) > 0 {
			for _, row := range rows {
				if row.Domain != "" && !cookieDomain.MatchString(row.Domain) {
					continue
				}
				put(row.Name, row.Value)
			}
		} else {
			var wrapped struct {
				Cookies []struct {
					Name   string `json:"name"`
					Value  string `json:"value"`
					Domain string `json:"domain"`
				} `json:"cookies"`
			}
			if err2 := json.Unmarshal([]byte(text), &wrapped); err2 == nil && len(wrapped.Cookies) > 0 {
				for _, row := range wrapped.Cookies {
					if row.Domain != "" && !cookieDomain.MatchString(row.Domain) {
						continue
					}
					put(row.Name, row.Value)
				}
			} else {
				return "", fmt.Errorf("Cookie 看起来是 JSON，但没有解析出 name/value；请粘贴请求头里 cookie 那一行的值（name=value; name=value; …）")
			}
		}
	} else {
		// Header form, table form, or a mix: split on ';' and newlines, then
		// drop tab-separated trailing columns.
		for _, part := range strings.FieldsFunc(text, func(r rune) bool {
			return r == ';' || r == '\n'
		}) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// Table paste: "name\tvalue\tdomain\tpath\t..."
			if strings.Contains(part, "\t") {
				cols := strings.Split(part, "\t")
				if len(cols) < 2 {
					continue
				}
				nameCol := strings.TrimSpace(cols[0])
				valueCol := strings.TrimSpace(cols[1])
				domainCol := ""
				if len(cols) > 2 {
					domainCol = strings.TrimSpace(cols[2])
				}
				// Skip a header row ("Name  Value  Domain").
				if strings.EqualFold(nameCol, "name") {
					continue
				}
				if domainCol != "" && !cookieDomain.MatchString(domainCol) {
					continue
				}
				put(nameCol, valueCol)
				continue
			}
			idx := strings.Index(part, "=")
			if idx <= 0 {
				continue
			}
			put(part[:idx], part[idx+1:])
		}
	}

	if len(pairs) == 0 {
		return "", fmt.Errorf("Cookie 里没有解析出任何 name=value；请粘贴请求头里 cookie 那一行的完整值")
	}
	var missing []string
	for _, name := range requiredCookies {
		if _, ok := pairs[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("Cookie 不完整，缺少 %s。请在开发者工具 Network 面板里，从发往 gemini.google.com 的请求的请求头复制 cookie 一行的完整值（不要从 Application 面板逐条复制）",
			strings.Join(missing, "、"))
	}

	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+pairs[name])
	}
	return strings.Join(out, "; "), nil
}

// ParseAuthUser validates the /u/N account index.
func ParseAuthUser(input string) (int, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return 0, nil
	}
	if !authuserPattern.MatchString(trimmed) {
		return 0, fmt.Errorf("账号索引须为 0–9 的整数：请填写复制 Cookie 的同一窗口中 /u/N/app 的 N；单账号会话可留空（0）")
	}
	return int(trimmed[0] - '0'), nil
}

// importAccountResponse handles POST /accounts.
func (m *Manager) importAccountResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	var body importRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return jsonResponse(http.StatusBadRequest, importAccountResponse{Error: "请求体不是合法 JSON：期望 {\"namespace\":\"ai-studio|gemini-web\", ...}"})
	}
	namespace := strings.TrimSpace(body.Namespace)
	if namespace == "" {
		return jsonResponse(http.StatusBadRequest, importAccountResponse{
			Error: "缺少 namespace：请填 \"ai-studio\"（AIStudio2API，浏览器登录）或 \"gemini-web\"（gemini-web2api-go，粘贴 Cookie）；" +
				"两个后端的加账号方式不同，插件不会替你猜",
		})
	}
	// Re-use the query selector so a form POST and a query-driven POST agree.
	req.Query = setNamespaceQuery(req.Query, namespace)

	fleet, err := m.fleetFor(req)
	if err != nil {
		return jsonResponse(statusForManagementError(err), importAccountResponse{Namespace: namespace, Error: err.Error()})
	}
	if fleet.external {
		return jsonResponse(http.StatusBadRequest, importAccountResponse{
			Namespace: fleet.namespace,
			Error:     fleet.namespace + ".base-url 指向外部后端：本插件无法向它导入账号，请在外部后端自己的管理页里导入",
		})
	}
	if fleet.manager == nil {
		return jsonResponse(http.StatusConflict, importAccountResponse{Namespace: fleet.namespace, Error: "插件尚未完成注册，请稍后再试"})
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	// Validate the pasted material BEFORE any backend round-trip: an operator
	// who pasted half a Cookie header should get the "which cookies are
	// missing" explanation even when the backend is not running yet.
	var authUser int
	var cookie string
	if fleet.flavor.ID == backend.FlavorGeminiWeb {
		var parseErr error
		authUser, parseErr = ParseAuthUser(body.AuthUser)
		if parseErr != nil {
			return jsonResponse(http.StatusBadRequest, importAccountResponse{Namespace: fleet.namespace, Error: parseErr.Error()})
		}
		cookie, parseErr = ParseCookieHeader(body.Cookie)
		if parseErr != nil {
			// The parsing error is written for the operator and contains no
			// cookie material.
			return jsonResponse(http.StatusBadRequest, importAccountResponse{Namespace: fleet.namespace, Error: parseErr.Error()})
		}
	} else if strings.TrimSpace(body.Cookie) != "" {
		return jsonResponse(http.StatusBadRequest, importAccountResponse{
			Namespace: fleet.namespace,
			Error: "AIStudio2API 不接受粘贴的 Cookie：它用自带的浏览器完成 Google 登录并把授权状态存在自己的数据目录里。" +
				"这里会启动它的登录流程（浏览器窗口由后端打开）；如果你要粘贴 Cookie，请改用 gemini-web 命名空间",
		})
	}

	ctx, cancel := contextWithTimeout(importTimeout)
	defer cancel()
	setup, setupErr := m.setupFor(ctx, cfg, fleet.namespace)
	if setupErr != nil {
		return jsonResponse(ensureHTTPStatus(setupErr), importAccountResponse{
			Namespace: fleet.namespace, Error: "本地后端未就绪：" + setupErr.Error(),
		})
	}

	switch fleet.flavor.ID {
	case backend.FlavorAIStudio:
		return m.importStudioAccount(ctx, fleet, setup, body)
	default:
		return m.importGeminiWebAccount(ctx, cfg, fleet, setup, body, cookie, authUser)
	}
}

// importStudioAccount starts AIStudio2API's own browser sign-in.
//
// AI Studio does not accept a pasted Cookie, so the plugin's job is to START its
// sign-in and report the task. A pasted cookie is refused (above) rather than
// posted at an endpoint that does not exist.
func (m *Manager) importStudioAccount(ctx context.Context, fleet namespaceFleet, setup backend.Setup, body importRequest) (pluginapi.ManagementResponse, error) {
	timeout := studioLoginDefaultTimeout
	if body.LoginTimeout > 0 {
		timeout = time.Duration(body.LoginTimeout) * time.Second
	}
	loginCtx, cancel := contextWithTimeout(timeout)
	defer cancel()
	task, err := backend.StartStudioLogin(loginCtx, m.httpDoer(), setup, timeout)
	if err != nil {
		return jsonResponse(ensureHTTPStatus(err), importAccountResponse{Namespace: fleet.namespace, Error: err.Error()})
	}
	return jsonResponse(http.StatusOK, importAccountResponse{
		OK:        true,
		Namespace: fleet.namespace,
		TaskID:    task.ID,
		TaskState: task.State,
		Detail:    "AI Studio 已开始浏览器登录；任务完成后请刷新账号数。登录在它的浏览器窗口里完成，插件不接触任何 Google 凭据",
	})
}

// importGeminiWebAccount pastes an already-validated Cookie into
// gemini-web2api-go's pool and probes it.
func (m *Manager) importGeminiWebAccount(ctx context.Context, cfg config.Config, fleet namespaceFleet, setup backend.Setup, body importRequest, cookie string, authUser int) (pluginapi.ManagementResponse, error) {
	nsCfg, cfgErr := cfg.Namespace(fleet.namespace)
	if cfgErr != nil {
		return jsonResponse(http.StatusBadRequest, importAccountResponse{Namespace: fleet.namespace, Error: cfgErr.Error()})
	}
	if proxy := nsCfg.Proxy; proxy != "" {
		// That backend does not read HTTP_PROXY: a proxy must be registered in
		// ITS pool before the sign-in check can reach Google.
		if _, proxyErr := backend.EnsureProxy(ctx, m.httpDoer(), setup, proxy); proxyErr != nil {
			return jsonResponse(http.StatusBadGateway, importAccountResponse{
				Namespace: fleet.namespace, Error: "无法把代理注册到本地后端：" + proxyErr.Error(),
			})
		}
	}

	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = "CPA " + timeNow().Format("2006-01-02")
	}

	created, importErr := backend.ImportCookie(ctx, m.httpDoer(), setup, cookie, label, authUser, importTimeout)
	if importErr != nil {
		// backend.ImportCookie already deletes a rejected account; its error
		// text is operator-facing and cookie-free.
		return jsonResponse(ensureHTTPStatus(importErr), importAccountResponse{Namespace: fleet.namespace, Error: importErr.Error()})
	}

	// A successful import is what makes the provider schedulable at all: the auth
	// record is written AFTER the account exists, so a failed import never
	// leaves a schedule-able-but-useless record behind.
	if errAuth := m.materializeAuth(context.Background()); errAuth != nil {
		_ = m.log("warn", "account imported but the auth record could not be written: "+errAuth.Error(), nil)
	}
	// Refresh that namespace's catalog: a model may now be covered by an account
	// when it was not before.
	refreshCtx, cancelRefresh := contextWithTimeout(registerRefreshTimeout)
	defer cancelRefresh()
	if refreshErr := m.refreshNamespace(refreshCtx, cfg, fleet.namespace); refreshErr != nil {
		_ = m.log("warn", "catalog refresh after account import failed: "+refreshErr.Error(), nil)
	}

	return jsonResponse(http.StatusOK, importAccountResponse{
		OK:        true,
		Namespace: fleet.namespace,
		AccountID: created.ID,
		Detail:    created.Detail,
	})
}
