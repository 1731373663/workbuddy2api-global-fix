// login.go — WorkBuddy OAuth login (CN + GLOBAL realms).
//
// Based on upstream Sliverkiss/workbuddy2api cmd/login (CN only), extended with
// the GLOBAL realm (workbuddy.ai) — same /v2/plugin/* endpoints, different base:
//
//	login url [cn|global]   → POST {base}/v2/plugin/auth/state?platform=CLI
//	login poll [cn|global]  → GET {base}/v2/plugin/auth/token?state=
//
// Global base/origin per Maquer/workbuddy-checkin login.sh:
//
//	GLOBAL_AUTH_BASE = https://www.workbuddy.ai (same path layout as CN)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	upstreamBaseCN     = "https://copilot.tencent.com"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	clientUA           = "CLI/2.63.2 CodeBuddy/2.63.2"
	originCN           = "https://www.codebuddy.cn"
	originGlobal       = "https://www.workbuddy.ai"

	// stateFileNameCN/Global 设备授权的 state 文件名（按 region 分开，避免
	// 同时开两个 region 的登录流程时互相覆盖）。
	stateFileNameCN     = "wb2api-login-state-cn.json"
	stateFileNameGlobal = "wb2api-login-state-global.json"
)

// stateDir 返回 state 文件所在目录，并确保目录存在。
//
// 为什么不硬编码绝对路径：原实现写死 `/tmp/...`，在 Windows 上不可用（PR 作者
// 的动机）；而 PR 改成写死 `C:/Users/Administrator/Desktop/Mod/...` 又反过来在
// Linux/macOS 上不可用（该路径不存在，会直接 fatal）。
// 这里用 os.TempDir()（Linux → /tmp，Windows → %TEMP%），两端都成立；
// 允许用 WB2A_STATE_DIR 环境变量覆盖，便于把 state 放到指定目录。
func stateDir() string {
	dir := os.Getenv("WB2A_STATE_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	// 目录不存在时尝试创建；失败则回落临时目录（写文件时会给出真实错误）。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return os.TempDir()
	}
	return dir
}

// region 规范化：接受 cn/global，大小写与空白容错，非法值报错。
//
// 默认 CN（注意：上游 PR 的默认是 global）。原因：login.sh 以 `login url`
// （不带参数）调用本工具，若默认改成 global，现有 CN 用户跑一次 ./login.sh
// 就会静默切到另一个 realm，登录的账号体系完全不同。故保持 CN 为默认，
// 确保既有流程行为零变化；需要 Global 时显式传 `global`。
func normalizeRegion(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "cn":
		return "cn", nil
	case "global":
		return "global", nil
	default:
		return "", fmt.Errorf("unknown region %q (want cn|global)", s)
	}
}

func bases(region string) (base, origin, stateFile string) {
	if region == "global" {
		return upstreamBaseGlobal, originGlobal, filepath.Join(stateDir(), stateFileNameGlobal)
	}
	return upstreamBaseCN, originCN, filepath.Join(stateDir(), stateFileNameCN)
}

func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State string `json:"state"`
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll> [cn|global] (default cn)")
	}
	sub := os.Args[1]
	// 缺省 CN：保证 login.sh 的既有调用（login url / login poll）行为不变。
	// 要用 Global realm 请显式传第二个参数（./login.sh global）。
	regionArg := "cn"
	if len(os.Args) >= 3 {
		regionArg = os.Args[2]
	}
	region, err := normalizeRegion(regionArg)
	if err != nil {
		fatal("%v", err)
	}
	base, origin, stateFile := bases(region)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch sub {
	case "url":
		h := commonHeaders(origin)
		data, _, err := doJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", h, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" {
			fatal("auth state: missing state (authUrl may be region-local login page)")
		}
		raw, _ := json.Marshal(loginState{State: st.State})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		// Upstream returns authUrl for CN sometimes empty on global; build fallback.
		url := st.AuthURL
		if url == "" {
			url = base + "/login?state=" + st.State + "&platform=CLI"
		}
		fmt.Println(url)

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (run `login url %s` first)", err, region)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		h := commonHeaders(origin)
		tokRaw, status, errTok := doJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+ls.State, h, nil)
		if errTok != nil {
			if status == 0 || status >= 500 {
				fatal("token endpoint error: %v", errTok)
			}
			fatal("login not completed yet. Finish browser login first, then re-run `login poll %s`", region)
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("login not completed yet. Finish browser login first, then re-run `login poll %s`", region)
		}
		acctHeaders := func(r *http.Request) {
			h(r)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+ls.State, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		if tok.Domain == "" && region == "global" {
			tok.Domain = "www.workbuddy.ai"
		}
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        tok.Domain,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
			"region":        region,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll [cn|global])", sub)
	}
}
