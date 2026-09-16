package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestIsGlobalRealm 区域判定：决定请求打到 workbuddy.ai 还是 copilot.tencent.com。
func TestIsGlobalRealm(t *testing.T) {
	cases := []struct {
		domain string
		want   bool
	}{
		{"www.workbuddy.ai", true},
		{"workbuddy.ai", true},
		{"https://www.workbuddy.ai", true},
		{"WWW.WORKBUDDY.AI", true},
		{"copilot.tencent.com", false},
		{"codebuddy.cn", false},
		{"", false}, // 空 domain（老/CN 凭证）→ CN，保持既有行为
		{"  ", false},
		{"evil-workbuddy.ai", false}, // 后缀匹配必须带点，防仿冒域名
		{"workbuddy.ai.evil.com", false},
	}
	for _, c := range cases {
		a := &auth.Auth{Domain: c.domain}
		if got := isGlobalRealm(a); got != c.want {
			t.Errorf("isGlobalRealm(domain=%q) = %v, want %v", c.domain, got, c.want)
		}
	}
	// nil 账号不应 panic。
	if isGlobalRealm(nil) {
		t.Error("nil 账号不应判为 Global")
	}
}

// TestChatBaseByRegion 聊天基址按 region 分流。
func TestChatBaseByRegion(t *testing.T) {
	c := New()

	global := &auth.Auth{Domain: "www.workbuddy.ai"}
	if got := c.chatBase(global); got != "https://www.workbuddy.ai" {
		t.Errorf("Global chatBase = %q", got)
	}
	cn := &auth.Auth{Domain: "copilot.tencent.com"}
	if got := c.chatBase(cn); got != "https://copilot.tencent.com" {
		t.Errorf("CN chatBase = %q", got)
	}
	// domain 缺失 → 回落 CN（老凭证行为不变）。
	if got := c.chatBase(&auth.Auth{}); got != "https://copilot.tencent.com" {
		t.Errorf("空 domain 应回落 CN, 得到 %q", got)
	}
}

// TestBillingBaseByRegion 计费基址按 region 分流。
func TestBillingBaseByRegion(t *testing.T) {
	c := New()

	if got := c.billingBase(&auth.Auth{Domain: "www.workbuddy.ai"}); got != "https://www.workbuddy.ai" {
		t.Errorf("Global billingBase = %q", got)
	}
	if got := c.billingBase(&auth.Auth{Domain: "copilot.tencent.com"}); got != "https://www.codebuddy.cn" {
		t.Errorf("CN billingBase = %q", got)
	}
}

// TestOriginRefererByRegion Origin/Referer 必须与 region 匹配，
// 否则上游会按跨站请求拒绝。
func TestOriginRefererByRegion(t *testing.T) {
	if got := originRefererFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != "https://www.workbuddy.ai" {
		t.Errorf("Global origin = %q", got)
	}
	if got := originRefererFor(&auth.Auth{Domain: "copilot.tencent.com"}); got != "https://www.codebuddy.cn" {
		t.Errorf("CN origin = %q", got)
	}
}
