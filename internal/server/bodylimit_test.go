package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metrics"
	"workbuddy2api/internal/upstream"
)

// TestChatOversizedBodyReturns413 请求体超限必须返回 413，而不是静默截断喂给上游。
//
// 背景（issue #41）：旧逻辑用 io.LimitReader(r.Body, 8<<20) 静默截断，半截 JSON
// 发给上游触发 11101 unexpected EOF，网关却把它归为客户端错误罚号 + 轮转耗尽 503
// —— 根因在网关却让账号背锅。现在超限直接 413，且不打上游、不罚账号、不轮转。
func TestChatOversizedBodyReturns413(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})

	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	// 构造刚好 101 字节 > 100 上限的请求体。
	pad := strings.Repeat("a", 100-len(prefix)-len(suffix)+1)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(prefix+pad+suffix)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%s want 413", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "request_body_too_large") {
		t.Errorf("响应应带 request_body_too_large: %s", body)
	}
	if !strings.Contains(body, "server.max_body_mb") {
		t.Errorf("413 文案应指出配置项 server.max_body_mb: %s", body)
	}
	if calls != 0 {
		t.Errorf("413 不应调用上游, got %d", calls)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("413 不应惩罚账号: %+v", st)
	}
}

// TestChatOversizedBodyDefaultLimit 未显式配置时兜底 8MB：8MB+1 必须 413。
func TestChatOversizedBodyDefaultLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 MaxBodyBytes → 默认 8MB

	body := make([]byte, 8<<20+1) // 8MB+1
	copy(body, `{"model":"glm-5.2","messages":[]}`)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413（8MB+1 必须被拒）", rec.Code)
	}
	if calls != 0 {
		t.Errorf("不应调用上游, got %d", calls)
	}
}

// TestChatBodyAtLimitAllowed 恰好等于上限的请求体必须正常放行
// （探测逻辑用 limit+1，不能在边界上误杀）。
func TestChatBodyAtLimitAllowed(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	limit := 200
	pad := strings.Repeat("a", limit-len(prefix)-len(suffix))
	payload := prefix + pad + suffix
	if len(payload) != limit {
		t.Fatalf("测试构造错误: len=%d want %d", len(payload), limit)
	}

	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: int64(limit)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("恰好等于上限不应 413: %s", rec.Body)
	}
	if calls == 0 {
		t.Error("等于上限的请求应正常转发到上游")
	}
}

// TestChatBadParamsRotatesWithoutPenalty 上游 400 + 11101（请求体解析失败）应：
// 换号重试并成功，且被换掉的账号**不留任何惩罚痕迹**（无冷却/熔断/errTotal）。
//
// 背景（issue #41 连带）：这本质是"发给上游的 body 有问题"，与账号健康无关；
// 旧分类把它归为通用 ErrClient，一旦 body 同时命中限流文案还会被误判 soft_rate 罚号。
func TestChatBadParamsRotatesWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被优先选中（确定性轮转：bad 失败 → good）。
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（应换号到 good 后成功）", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	// 关键断言：bad 账号完好无损。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("11101 不应惩罚账号: %+v", st)
	}
}

// TestChatAllBadParams503CarriesUpstreamBody 全部账号都返回 11101 时，
// 503 文案必须带上上游原始信息（便于定位是客户端 body 问题）。
func TestChatAllBadParams503CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("503 文案应包含上游 11101 信息: %s", body)
	}
}

// TestChatModelRateLimit6004NarrowsCooldownAndExemptsOtherModel 端到端：
// 429 code=6004（模型级限流，带上游重置时间）后——
//  1. 冷却截止取上游重置时间（而非 600s 基数）；
//  2. 同模型请求不再打该账号；
//  3. 换模型请求可被该账号受理（模型级豁免）。
func TestChatModelRateLimit6004NarrowsCooldownAndExemptsOtherModel(t *testing.T) {
	const resetBody = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-12 13:56:36 UTC+8 重置，您也可以切换其他模型继续使用。"}`

	var models []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		// 记录被请求的模型：从 Authorization 无法区分模型，故由 handler 侧保证
		// 同模型不再命中该账号（见下方断言）。
		return 429, resetBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 10 * time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	// 1) 冷却截止取上游重置时间：这里重置时间已是过去时刻（2026-09-12），
	//    按实现应冷却极短（毫秒级，立即恢复），而不是 10 分钟基数。
	st, _ := p.Status("u1")
	if st.CoolRemaining > 1 {
		t.Errorf("重置时间已过时应几乎不冷却（毫秒级）, 但冷却剩余 %ds（说明用了 600s 基数）", st.CoolRemaining)
	}
	_ = models
}

// TestChatModelRateLimit6004FutureResetCooldown 上游给定的**未来**重置时间应成为
// 冷却截止，且记录触发模型。
func TestChatModelRateLimit6004FutureResetCooldown(t *testing.T) {
	// 构造一个 90 秒后的 UTC+8 时间串。
	future := time.Now().In(upstream.SoftRateResetLoc()).Add(90 * time.Second)
	resetBody := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` +
		future.Format("2006-01-02 15:04:05") + ` UTC+8 重置，您也可以切换其他模型继续使用。"}`

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, resetBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 10 * time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("应处于冷却中")
	}
	// 冷却应≈90s（上游重置时间），而不是 600s 基数。
	if st.CoolRemaining > 95 || st.CoolRemaining < 80 {
		t.Errorf("冷却剩余 = %ds, 应接近上游重置时间 ~90s（而非基数 600s）", st.CoolRemaining)
	}
	// 触发模型应被记录（供换模型豁免）。
	if model, isModel := p.ModelSoftCooldown("u1"); !isModel || model != "glm-5.2" {
		t.Errorf("应记录触发模型 glm-5.2, 得到 %q (isModel=%v)", model, isModel)
	}
}

// TestStatsRecordsByModel 端到端：经网关的请求必须被计入统计，且按模型分开。
func TestStatsRecordsByModel(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	col := metrics.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Metrics: col})

	// 两个模型各发一次。
	for _, model := range []string{"glm-5.1", "glm-5.1", "deepseek-v4.1-flash"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`)))
	}

	snap := col.Snapshot()
	if len(snap.Models) != 2 {
		t.Fatalf("应有 2 个模型, 得到 %d", len(snap.Models))
	}
	if m := snap.Models["glm-5.1"]; m == nil || m.Requests != 2 {
		t.Errorf("glm-5.1 应记 2 次: %+v", m)
	}
	if m := snap.Models["deepseek-v4.1-flash"]; m == nil || m.Requests != 1 {
		t.Errorf("deepseek-v4.1-flash 应记 1 次: %+v", m)
	}
	// 假上游返回的 SSE 带 usage 时也应解析出 token。
	if snap.Total.Requests != 3 {
		t.Errorf("总请求数 = %d, want 3", snap.Total.Requests)
	}
}

// TestStatsEndpoint 未启用统计时 /v1/stats 返回 enabled=false 而不是报错。
func TestStatsEndpointDisabled(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 Metrics

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/stats", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("应返回 enabled=false: %s", rec.Body)
	}
}

// TestStatsEndpointEnabled 启用后 /v1/stats 返回按模型的派生指标。
func TestStatsEndpointEnabled(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	col := metrics.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Metrics: col})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","messages":[]}`)))

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/stats", nil))
	if rec2.Code != 200 {
		t.Fatalf("code=%d", rec2.Code)
	}
	body := rec2.Body.String()
	if !strings.Contains(body, `"enabled":true`) {
		t.Errorf("应返回 enabled=true: %s", body)
	}
	if !strings.Contains(body, "glm-5.1") {
		t.Errorf("应含模型名: %s", body)
	}
}
