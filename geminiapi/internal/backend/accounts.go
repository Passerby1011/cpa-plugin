package backend

// Each backend's own account pool, driven through its own admin surface.
//
// # Two surfaces, two ways to add an account
//
// The two backends do NOT share an account API, and treating them as
// interchangeable is what made an earlier implementation import a Google cookie
// into a service that has no cookie endpoint:
//
//   - gemini-web2api-go owns the Google cookie itself. Its /admin/api/cookies
//     takes a pasted Cookie header, stores it in its own SQLite database and can
//     then check that it signs in. A cookie is the account.
//
//   - AIStudio2API never accepts a pasted cookie. It signs accounts in with a
//     browser (its own Camoufox build) and keeps the resulting auth state under
//     its data directory. POST /api/accounts starts that sign-in and returns a
//     task; GET /api/accounts/tasks reports progress. There is no cookie
//     endpoint to post to.
//
// So "import an account" means one thing per flavor, and the two functions
// below are deliberately not variations of one another.
//
// # What lives here and what does NOT
//
// The backend owns the Google credential: gemini-web writes the cookie into
// its database, AI Studio writes its auth state into its own directory. This
// package only ever passes a cookie THROUGH — in a request body to 127.0.0.1 —
// and never keeps, logs or returns one. Every function's error text is
// deliberately free of credential material.

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

// adminProxy is one entry of gemini-web2api-go's proxy pool.
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

// EnsureProxy registers a proxy in gemini-web2api-go's own pool and returns
// its id.
//
// This matters because that backend does NOT read HTTP_PROXY: it only reaches
// Google directly or through a proxy registered here. An operator who filled in
// the proxy on the plugin page would otherwise see every account check fail with
// a misleading "cannot reach Google".
func EnsureProxy(ctx context.Context, doer HTTPDoer, setup Setup, proxyURL string) (int, error) {
	if setup.Flavor == FlavorAIStudio {
		// AI Studio takes its proxy from .env (PROXY), which EnsureStudioFiles
		// already writes; there is no pool to populate. Saying so is better
		// than silently posting to an endpoint that does not exist.
		return 0, fmt.Errorf("AIStudio2API takes its proxy from its own .env (the PROXY key), not from a pool: set it in the plugin config and the manager writes it")
	}
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

// ImportCookie adds one Google cookie to gemini-web2api-go's pool and probes
// it. It is refused for AI Studio, which has no cookie endpoint at all.
//
// The probe is what distinguishes "added" from "works": a cookie that is
// syntactically fine but cannot sign in would otherwise sit in the pool and
// fail every request routed to it. A rejected account is DELETED again, so the
// pool never accumulates dead entries.
//
// The returned error is written for the operator and never contains the cookie.
func ImportCookie(ctx context.Context, doer HTTPDoer, setup Setup, cookie, label string, authUser int, timeout time.Duration) (ImportedAccount, error) {
	if setup.Flavor != FlavorGeminiWeb {
		return ImportedAccount{}, fmt.Errorf("%s does not accept a pasted Cookie: it signs accounts in through its own browser (see StartStudioLogin)", flavorLabelOr(setup.Flavor))
	}
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
		return ImportedAccount{}, fmt.Errorf("这个 Cookie 无法登录 Gemini：%s", RedactAccountDetail(detail))
	}
	return ImportedAccount{ID: added.ID, Detail: RedactAccountDetail(check.Detail)}, nil
}

// StudioLoginTask is what AIStudio2API reports for a sign-in it is performing.
//
// The sign-in is a real browser session against Google, so it is asynchronous
// by nature: the POST returns a task, and the task moves from pending/running
// to done or failed. Nothing here carries account material — a task id, a state
// and an operator-facing error are all this plugin ever sees.
type StudioLoginTask struct {
	ID    string
	State string
	Error string
}

// StartStudioLogin starts AIStudio2API's own account sign-in.
//
// This is AI Studio's ONLY way to add an account: it drives its own browser and
// stores the resulting auth state itself. A pasted cookie is not an input it
// accepts, so the plugin must not pretend otherwise.
//
// The call is bounded by loginTimeout because upstream's own sign-in may wait
// for a human to complete a Google flow in the browser window it opens.
func StartStudioLogin(ctx context.Context, doer HTTPDoer, setup Setup, timeout time.Duration) (StudioLoginTask, error) {
	if setup.Flavor != FlavorAIStudio {
		return StudioLoginTask{}, fmt.Errorf("%s adds accounts by pasting a Cookie; POST /accounts is the cookie path", flavorLabelOr(setup.Flavor))
	}
	if !setup.HasAdminCredentials() {
		return StudioLoginTask{}, fmt.Errorf("这个后端没有管理凭据：只有插件自己托管的后端才能加账号")
	}
	var res struct {
		Task struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"task"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodPost, "/api/accounts", struct{}{}, timeout,
		"AI Studio 没有在限定时间内开始登录任务", &res); err != nil {
		return StudioLoginTask{}, err
	}
	if res.Task.ID != "" {
		return StudioLoginTask{ID: res.Task.ID, State: res.Task.State}, nil
	}
	if res.Account.ID != "" {
		// Upstream returns the account directly when no browser is needed.
		return StudioLoginTask{ID: res.Account.ID, State: "done"}, nil
	}
	return StudioLoginTask{}, fmt.Errorf("AI Studio 没有返回登录任务 id")
}

// PollStudioLogin reads one sign-in task's current state.
func PollStudioLogin(ctx context.Context, doer HTTPDoer, setup Setup, taskID string) (StudioLoginTask, bool, error) {
	if setup.Flavor != FlavorAIStudio {
		return StudioLoginTask{}, false, fmt.Errorf("%s has no login tasks", flavorLabelOr(setup.Flavor))
	}
	if strings.TrimSpace(taskID) == "" {
		return StudioLoginTask{}, false, fmt.Errorf("缺少登录任务 id")
	}
	var res struct {
		Tasks []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Error string `json:"error"`
			Items []struct {
				State string `json:"state"`
				Error string `json:"error"`
			} `json:"items"`
		} `json:"tasks"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodGet, "/api/accounts/tasks", nil, 10*time.Second, "", &res); err != nil {
		return StudioLoginTask{}, false, err
	}
	for _, task := range res.Tasks {
		if task.ID != taskID {
			continue
		}
		out := StudioLoginTask{ID: task.ID, State: task.State, Error: RedactAccountDetail(task.Error)}
		for _, item := range task.Items {
			if item.State == "failed" && out.Error == "" {
				out.Error = RedactAccountDetail(item.Error)
			}
		}
		done := task.State == "done" || task.State == "failed"
		return out, done, nil
	}
	// A task that disappeared between two polls has finished and been reaped;
	// reporting "gone" beats polling forever.
	return StudioLoginTask{}, true, fmt.Errorf("AI Studio 已经不再报告这个登录任务（%s），它可能已结束并被执行器回收", taskID)
}

// ReadAccountCounts reads the pool summary.
//
// It reports COUNTS only: gemini-web's list rows carry labels and account email
// addresses, AI Studio's carry labels and auth-state ids, and a page refresh has
// no business echoing any of them.
func ReadAccountCounts(ctx context.Context, doer HTTPDoer, setup Setup) (AccountCounts, error) {
	if setup.Flavor == FlavorAIStudio {
		return studioAccountCounts(ctx, doer, setup)
	}
	return geminiWebAccountCounts(ctx, doer, setup)
}

func geminiWebAccountCounts(ctx context.Context, doer HTTPDoer, setup Setup) (AccountCounts, error) {
	var list struct {
		Accounts []struct {
			ID      int  `json:"id"`
			Enabled bool `json:"enabled"`
		} `json:"accounts"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodGet, "/accounts", nil, 5*time.Second, "", &list); err != nil {
		return AccountCounts{}, fmt.Errorf("read the account pool: %w", err)
	}
	out := AccountCounts{Total: len(list.Accounts), Known: true}
	for _, account := range list.Accounts {
		if account.Enabled {
			out.Enabled++
		}
	}
	return out, nil
}

func studioAccountCounts(ctx context.Context, doer HTTPDoer, setup Setup) (AccountCounts, error) {
	var list struct {
		Accounts []struct {
			Enabled *bool `json:"enabled"`
		} `json:"accounts"`
	}
	if err := adminCallJSON(ctx, doer, setup, http.MethodGet, "/api/accounts", nil, 10*time.Second, "", &list); err != nil {
		return AccountCounts{}, fmt.Errorf("read the account pool: %w", err)
	}
	out := AccountCounts{Total: len(list.Accounts), Known: true}
	for _, account := range list.Accounts {
		// Upstream omits "enabled" for an account that is usable; only an
		// explicit false means disabled.
		if account.Enabled == nil || *account.Enabled {
			out.Enabled++
		}
	}
	return out, nil
}

// flavorLabelOr is the operator-facing name of a flavor id, tolerant of an
// empty one.
func flavorLabelOr(id FlavorID) string {
	if flavor, err := FlavorFor(id); err == nil {
		return flavor.Label()
	}
	return "this backend"
}

// RedactAccountDetail keeps a backend verdict readable while making sure it
// cannot carry credential material: a detail string is a verdict sentence, not a
// payload, so anything long or containing an '=' pair is dropped.
func RedactAccountDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if len(detail) > 200 || strings.Contains(detail, "SID=") || strings.Contains(detail, "SAPISID=") || strings.Contains(detail, "aistudio_admin=") {
		return "后端返回了一条已隐藏的详情（含疑似凭据内容）"
	}
	return detail
}

// redactAccountDetail is the pre-existing lowercase spelling, kept so callers
// inside this package read naturally.
func redactAccountDetail(detail string) string { return RedactAccountDetail(detail) }
