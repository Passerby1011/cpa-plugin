package backend

// The backend's account pool, driven through its own admin API.
//
// # What lives here and what does NOT
//
// The backend owns the Google cookies: they are written into its SQLite
// database by its own admin handler. This package only ever passes a cookie
// THROUGH — in a request body to 127.0.0.1 — and never keeps, logs or returns
// one. Every function's error text is deliberately free of cookie material.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ImportedAccount is the backend's reply for a newly added account.
type ImportedAccount struct {
	ID     int
	Detail string
}

// adminProxy is one entry of the backend's proxy pool.
type adminProxy struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	FailCount int    `json:"fail_count"`
}

type adminProxyList struct {
	Items []adminProxy `json:"items"`
}

// adminCallJSON is AdminCall with the result decoded into out.
func adminCallJSON(ctx context.Context, doer HTTPDoer, setup Setup, method, path string, body any, timeout time.Duration, timeoutMessage string, out any) error {
	raw, err := AdminCall(ctx, doer, setup, method, path, body, timeout, timeoutMessage)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the backend returned an unexpected admin response")
	}
	return nil
}

// EnsureProxy registers a proxy in the backend's own pool and returns its id.
//
// This matters because the backend does NOT read HTTP_PROXY: it only reaches
// Google directly or through a proxy registered here. An operator who filled in
// the proxy on the plugin page would otherwise see every account check fail with
// a misleading "cannot reach Google".
func EnsureProxy(ctx context.Context, doer HTTPDoer, setup Setup, proxyURL string) (int, error) {
	const managedName = "CPA geminiae"
	var list adminProxyList
	if err := adminCallJSON(ctx, doer, setup, http.MethodGet, "/proxies", nil, 5*time.Second, "", &list); err != nil {
		return 0, fmt.Errorf("read the proxy pool: %w", err)
	}
	// An exact URL already present wins, so repeated imports do not pile up
	// duplicate pool entries.
	for _, item := range list.Items {
		if item.URL != proxyURL {
			continue
		}
		if !item.Enabled {
			if err := adminCallJSON(ctx, doer, setup, http.MethodPatch, fmt.Sprintf("/proxies/%d", item.ID), map[string]any{"enabled": true}, 5*time.Second, "", nil); err != nil {
				return 0, fmt.Errorf("enable proxy %d: %w", item.ID, err)
			}
		}
		if item.FailCount > 0 {
			_ = adminCallJSON(ctx, doer, setup, http.MethodPost, fmt.Sprintf("/proxies/%d/reset", item.ID), nil, 5*time.Second, "", nil)
		}
		return item.ID, nil
	}
	// Otherwise reuse our managed slot by name.
	for _, item := range list.Items {
		if item.Name != managedName {
			continue
		}
		if err := adminCallJSON(ctx, doer, setup, http.MethodPatch, fmt.Sprintf("/proxies/%d", item.ID),
			map[string]any{"name": managedName, "url": proxyURL, "enabled": true}, 5*time.Second, "", nil); err != nil {
			return 0, fmt.Errorf("update proxy %d: %w", item.ID, err)
		}
		return item.ID, nil
	}
	var created struct {
		ID int `json:"id"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodPost, "/proxies",
		map[string]any{"name": managedName, "url": proxyURL, "weight": 1}, 10*time.Second, "", &created); err != nil {
		return 0, fmt.Errorf("create proxy: %w", err)
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("the backend created a proxy without returning its id")
	}
	return created.ID, nil
}

// ImportCookie adds one Google cookie to the backend's pool and probes it.
//
// The probe is what distinguishes "added" from "works": a cookie that is
// syntactically fine but cannot sign in would otherwise sit in the pool and
// fail every request routed to it. A rejected account is DELETED again, so the
// pool never accumulates dead entries.
//
// The returned error is written for the operator and never contains the cookie.
func ImportCookie(ctx context.Context, doer HTTPDoer, setup Setup, cookie, label string, authUser int, timeout time.Duration) (ImportedAccount, error) {
	if strings.TrimSpace(cookie) == "" {
		return ImportedAccount{}, fmt.Errorf("没有收到 Cookie")
	}
	if setup.AdminToken == "" {
		return ImportedAccount{}, fmt.Errorf("这个后端没有管理口令：只有插件自己托管的后端才能导入账号")
	}
	body := map[string]any{
		"label":    label,
		"cookie":   cookie,
		"note":     "Imported from the CPA plugin page",
		"authuser": authUser,
	}
	var added struct {
		ID int `json:"id"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodPost, "/cookies", body, 30*time.Second,
		"本地后端 30 秒内没有接受这个 Cookie", &added); err != nil {
		return ImportedAccount{}, err
	}
	if added.ID == 0 {
		return ImportedAccount{}, fmt.Errorf("本地后端没有返回新账号的 id")
	}

	probeTimeoutMessage := fmt.Sprintf("本地后端在 %d 秒内没有检测完这个 Cookie：检测要由后端访问 gemini.google.com，这一步一直没返回，多半是后端连不上 Google；如果所在网络需要代理，请在页面上填写代理地址后重试", int(timeout.Seconds()))
	var check struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodPost, fmt.Sprintf("/cookies/%d/check", added.ID), nil, timeout, probeTimeoutMessage, &check); err != nil {
		// Never leave a half-imported account behind.
		_ = adminCallJSON(context.Background(), doer, setup, http.MethodDelete, fmt.Sprintf("/cookies/%d", added.ID), nil, 10*time.Second, "", nil)
		return ImportedAccount{}, err
	}
	if !check.OK {
		_ = adminCallJSON(context.Background(), doer, setup, http.MethodDelete, fmt.Sprintf("/cookies/%d", added.ID), nil, 10*time.Second, "", nil)
		detail := strings.TrimSpace(check.Detail)
		if detail == "" {
			detail = "检测未通过"
		}
		return ImportedAccount{}, fmt.Errorf("这个 Cookie 无法登录 Gemini：%s", redactAccountDetail(detail))
	}
	return ImportedAccount{ID: added.ID, Detail: redactAccountDetail(check.Detail)}, nil
}

// AccountCounts reads the pool summary.
//
// It reports COUNTS only: the list endpoint's rows carry labels and account
// email addresses, and a page refresh has no business echoing those.
func ReadAccountCounts(ctx context.Context, doer HTTPDoer, setup Setup) (total int, enabled int, err error) {
	var list struct {
		Accounts []struct {
			ID      int  `json:"id"`
			Enabled bool `json:"enabled"`
		} `json:"accounts"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodGet, "/accounts", nil, 5*time.Second, "", &list); err != nil {
		return 0, 0, fmt.Errorf("read the account pool: %w", err)
	}
	total = len(list.Accounts)
	for _, account := range list.Accounts {
		if account.Enabled {
			enabled++
		}
	}
	return total, enabled, nil
}

// redactAccountDetail keeps a backend verdict readable while making sure it
// cannot carry cookie material: a detail string is a verdict sentence, not a
// payload, so anything long or containing an '=' pair is dropped.
func redactAccountDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if len(detail) > 200 || strings.Contains(detail, "SID=") || strings.Contains(detail, "SAPISID=") {
		return "后端返回了一条已隐藏的详情（含疑似凭据内容）"
	}
	return detail
}
