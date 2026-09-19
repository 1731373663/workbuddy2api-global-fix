package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// pinnedTestHandler 构造 Session=nil（会话粘性关闭）的 handler，池内预置一个账号。
// ExpiresAt=0 使 NeedsRefresh 恒真，从而稳定走到 refresh 失败 → fail() 分支。
func pinnedTestHandler(t *testing.T, uid string) *Handler {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{
		UID:          uid,
		AccessToken:  "fake-access-token",
		RefreshToken: "fake-refresh-token",
	})
	return NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		Session:  nil, // config: session_sticky.enabled = false
	})
}

// TestPinnedChatDoesNotPanicWithoutSession 回归测试：
// 会话粘性关闭（Session=nil）时，账号固定端点 /v1/a/<uid>/chat/completions
// 在账号请求失败时曾 nil 解引用 panic（session.(*Router).Unbind(0x0, ...)），
// 直接打崩整个网关进程。修复后必须返回错误响应而非 panic。
func TestPinnedChatDoesNotPanicWithoutSession(t *testing.T) {
	const uid = "11111111-2222-3333-4444-555555555555"
	h := pinnedTestHandler(t, uid)

	req := httptest.NewRequest(http.MethodPost, "/v1/a/"+uid+"/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// 不应 panic；返回 5xx（凭据是假的，上游调用必然失败）。
	h.ServeHTTP(w, req)

	if w.Code == 0 {
		t.Fatal("没有任何响应")
	}
	if w.Code < 400 {
		t.Errorf("假凭据不应成功, 状态码 = %d", w.Code)
	}
}

// TestPinnedChatUnknownAccountWithoutSession 未知账号（404 分支）同样不应 panic。
func TestPinnedChatUnknownAccountWithoutSession(t *testing.T) {
	h := pinnedTestHandler(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	req := httptest.NewRequest(http.MethodPost, "/v1/a/does-not-exist/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("未知账号应返回 404, 得到 %d", w.Code)
	}
}

// TestUnbindSessionNilSafe 直接验证 Session=nil 时绑定/解绑路径不 panic。
// handler 内部对 cfg.Session 做 nil 判空；这里用同一入口触发路径。
func TestUnbindSessionNilSafe(t *testing.T) {
	h := NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: upstream.New(),
		Session:  nil,
	})
	if h.cfg.Session != nil {
		t.Fatal("Session should be nil for this test")
	}
	// 走真实 chat 路径：Session=nil 时不得 panic（无可用账号时返回 503）。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == 0 {
		t.Fatal("no response written")
	}
}
