// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metrics"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// Metrics 请求统计收集器（可选；nil = 不统计）。
	// 网关是所有流量（含非面板客户端）的唯一必经点，统计在此采集。
	Metrics *metrics.Collector
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// Per-account pinned endpoints. A gateway that wants to round-robin at
	// ITS layer (e.g. 9Router with N connections) needs one target per
	// account, otherwise every connection would race for the same shared
	// pool Pick(). /v1/a/<uid>/... pins every request to that account.
	h.mux.HandleFunc("POST /v1/a/{uid}/chat/completions", h.withAuth(h.pinnedChat))
	h.mux.HandleFunc("GET /v1/a/{uid}/quota", h.withAuth(h.pinnedQuota))
	h.mux.HandleFunc("GET /v1/accounts", h.withAuth(h.accounts))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 请求统计：所有经本网关的请求（含绕过面板的客户端）按模型聚合。
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))
	h.mux.HandleFunc("GET /v1/quota", h.withAuth(h.quota))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// 会话粘性安全包装
//
// h.cfg.Session 可为 nil（config 里 session_sticky.enabled=false）。原实现只在
// 「提取会话键」处判了 nil，而解绑/绑定路径直接调 h.cfg.Session.Unbind/Bind ——
// 一旦 Session 为 nil 且走到这些分支就会 nil 解引用 panic，整个网关进程崩溃。
//
// 触发条件在 /v1/a/<uid>/chat/completions（账号固定端点）上尤其容易满足：
// 该端点把 uid 直接当 stickyUID 用，账号 token 需刷新或请求失败时会走 fail()，
// 而 fail() 内部就是 Unbind。即「关闭会话粘性 + 使用固定号端点 + 该号请求失败」
// 三个条件同时成立即崩。故所有会话操作统一走下面这层 nil-safe 包装。
// ---------------------------------------------------------------------------

// bindSession 绑定会话；Session 为 nil（粘性关闭）时静默跳过。
func (h *Handler) bindSession(key, uid string) {
	if h.cfg.Session == nil || key == "" || uid == "" {
		return
	}
	h.cfg.Session.Bind(key, uid)
}

// unbindSession 解绑会话；Session 为 nil 时静默跳过。
func (h *Handler) unbindSession(key string) {
	if h.cfg.Session == nil || key == "" {
		return
	}
	h.cfg.Session.Unbind(key)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

// stats 返回按模型聚合的请求统计（面板「统计」页数据源）。
//
// 采集点在这里而不是面板：网关是所有流量（含绕过面板的客户端）的唯一必经点，
// 只有在网关侧才能统计到完整调用，且不依赖面板是否在运行。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "统计未启用（server.metrics_enabled=false）",
		})
		return
	}
	snap := h.cfg.Metrics.Derived()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"since":      snap.Since,
		"now":        snap.Now,
		"uptime_sec": snap.UptimeSec,
		"total":      snap.Total,
		"models":     snap.Models,
	})
}

// statsReset 清空统计（运维手动归零，便于观察某个时间点之后的增量）。
func (h *Handler) statsReset(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "统计未启用"})
		return
	}
	h.cfg.Metrics.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "统计已重置"})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// quota reports per-account credit packages (used/total/remaining/reset) in
// the 9Router dashboard quota shape: {plan, quotas:{<name>:{used,total,
// remaining,resetAt,unlimited,recurring}}}. Fetched live from the upstream
// billing endpoint for every healthy account in the pool.
// accounts lists every pool member with its pinned endpoint, so an upstream
// gateway (9Router etc.) can create one connection per account and round-robin
// across them instead of sharing a single pooled connection.
func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	list := h.cfg.Pool.List()
	type acct struct {
		UID       string `json:"uid"`
		Nickname  string `json:"nickname,omitempty"`
		Credits   int64  `json:"credits"`
		Cooling   bool   `json:"cooling"`
		Disabled  bool   `json:"disabled"`
		ChatPath  string `json:"chat_path"`
		QuotaPath string `json:"quota_path"`
		CoolUntil string `json:"cool_until,omitempty"`
		Remaining string `json:"cool_remaining,omitempty"`
	}
	out := make([]acct, 0, len(list))
	for _, st := range list {
		a := acct{
			UID:       st.UID,
			Nickname:  st.Nickname,
			Credits:   st.Credits,
			Cooling:   st.Cooling,
			Disabled:  st.Disabled,
			ChatPath:  "/v1/a/" + st.UID + "/chat/completions",
			QuotaPath: "/v1/a/" + st.UID + "/quota",
		}
		if st.Cooling && !st.Until.IsZero() {
			a.CoolUntil = st.Until.Format(time.RFC3339)
			a.Remaining = time.Until(st.Until).Round(time.Second).String()
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": "workbuddy", "accounts": out})
}

// pinnedChat serves /v1/a/<uid>/chat/completions by pinning to that account.
func (h *Handler) pinnedChat(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing account uid")
		return
	}
	if h.cfg.Pool.PeekByUID(uid) == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	h.chatCompletions(w, r)
}

// pinnedQuota serves /v1/a/<uid>/quota — same payload as /v1/quota but only
// that one account, so a per-account connection reports its own credits.
func (h *Handler) pinnedQuota(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing account uid")
		return
	}
	// 复用缓存的全量结果再过滤，避免每个 per-account 连接都触发一轮全池探测
	// （N 个连接 × N 个账号 = N² 次上游调用）。
	all := h.cachedAccountQuotas()
	for _, a := range all {
		if a.UID == uid {
			writeJSON(w, http.StatusOK, map[string]any{
				"provider": "workbuddy",
				"accounts": []accountQuota{a},
			})
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
}

// quotaProbeTimeout 单次配额探测的总预算：所有账号探测共享这一个 deadline。
//
// 为什么需要总预算：上游单次调用超时是 120s（Client.HTTP.Timeout），若按账号串行探测，
// N 个账号最坏要 N×120s（46 账号 ≈ 92 分钟），请求早已被客户端/反代掐断，连接与
// 上游配额也被白白占用。这里给整体探测一个上限并并发执行，超时的账号按"未知"返回。
const quotaProbeTimeout = 15 * time.Second

// quotaCacheTTL /v1/quota 结果的缓存时长。
//
// 为什么必须缓存：配额看板通常按秒级轮询，而每次调用都会向上游逐个账号打 billing 接口。
// 无缓存时多账号 + 高频轮询会直接触发上游限流。缓存一份短 TTL 结果，既保证数据接近实时，
// 又把上游调用量压到「每 TTL 一次」。
const quotaCacheTTL = 30 * time.Second

// quotaCache 缓存上一次配额探测结果（按 UID 索引）。
var quotaCache struct {
	sync.RWMutex
	rows []accountQuota
	at   time.Time
}

func (h *Handler) quota(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "workbuddy",
		"accounts": h.cachedAccountQuotas(),
	})
}

// cachedAccountQuotas 返回缓存结果；缓存过期或为空时重新探测。
func (h *Handler) cachedAccountQuotas() []accountQuota {
	quotaCache.RLock()
	if len(quotaCache.rows) > 0 && time.Since(quotaCache.at) < quotaCacheTTL {
		rows := quotaCache.rows
		quotaCache.RUnlock()
		return rows
	}
	quotaCache.RUnlock()

	rows := h.buildAccountQuotas()
	quotaCache.Lock()
	quotaCache.rows = rows
	quotaCache.at = time.Now()
	quotaCache.Unlock()
	return rows
}

// buildAccountQuotas returns one quota row per pool account (shared by /v1/quota
// and the per-account /v1/a/<uid>/quota endpoint).
//
// 账号间并发探测、整体受 quotaProbeTimeout 约束：慢/挂住的账号不会拖垮整个响应。
func (h *Handler) buildAccountQuotas() []accountQuota {
	accounts := h.cfg.Pool.List()
	out := make([]accountQuota, len(accounts))

	// 整体 deadline：用带超时的 context 逐账号约束探测调用。
	ctx, cancel := context.WithTimeout(context.Background(), quotaProbeTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for i, st := range accounts {
		aq := accountQuota{
			UID:          st.UID,
			Nickname:     st.Nickname,
			Credits:      st.Credits,
			Cooling:      st.Cooling,
			CoolKind:     st.CoolKind,
			Reason:       st.Reason,
			Disabled:     st.Disabled,
			SuccessCount: st.SuccessCount,
			ErrTotal:     st.ErrTotal,
		}
		if st.Cooling && !st.Until.IsZero() {
			aq.CoolUntil = st.Until.Format(time.RFC3339)
			aq.CoolRemaining = formatRemaining(time.Until(st.Until))
		}
		out[i] = aq

		a := h.cfg.Pool.PeekByUID(st.UID)
		if a == nil {
			out[i].Error = "account not in pool; no credential available for a quota probe"
			continue
		}
		// Skip the live billing probe while the account is cooling — upstream
		// is already throttling it and another call just extends the 429s.
		if st.Cooling {
			out[i].Error = "cooling: quota probe skipped to avoid extending rate limit"
			continue
		}

		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			// 每个探测独立成 goroutine；写自己的下标，无数据竞争。
			pkgs, err := h.cfg.Upstream.ResourcePackages(a)
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Quotas = make(map[string]upstream.ResourcePackage, len(pkgs))
			seen := map[string]int{}
			for _, p := range pkgs {
				name := p.PackageName
				seen[name]++
				if seen[name] > 1 {
					name = fmt.Sprintf("%s %d", name, seen[name])
				}
				out[i].Quotas[name] = p
			}
		}(i, a)
	}

	// 等待全部探测完成或整体超时；超时后未完成的账号标记为"未知"，
	// goroutine 因各自 HTTP 超时最终会退出，不会泄漏。
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		for i := range out {
			if out[i].Quotas == nil && out[i].Error == "" {
				out[i].Error = "quota probe timed out"
			}
		}
	}
	return out
}

// formatRemaining 把冷却剩余时长格式化成 "2h 13m 05s" 风格，
// 便于看板直接展示（与 ISO 截止时间并列给出）。
func formatRemaining(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// accountQuota is one pool account's credit/cooling snapshot.
type accountQuota struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits  int64  `json:"credits"`
	Cooling  bool   `json:"cooling"`
	CoolKind string `json:"cool_kind,omitempty"`
	// ISO deadline of the active cooldown ("until"), plus pre-formatted
	// "2h 13m 05s"-style remaining time so dashboards can render both.
	CoolUntil     string                              `json:"cool_until,omitempty"`
	CoolRemaining string                              `json:"cool_remaining,omitempty"`
	Reason        string                              `json:"reason,omitempty"`
	Disabled      bool                                `json:"disabled"`
	SuccessCount  int64                               `json:"success_count,omitempty"`
	ErrTotal      int64                               `json:"err_total,omitempty"`
	Quotas        map[string]upstream.ResourcePackage `json:"quotas"`
	Error         string                              `json:"error,omitempty"`
}

// 静态模型表（api-reference §5 回退 + WorkBuddy GLOBAL catalog 2026-09-07）。
var staticModels = []map[string]any{
	{"id": "hy4-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "gpt-5.6-sol", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 400000},
	{"id": "gpt-5.6-terra", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 400000},
	{"id": "gpt-5.6-luna", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 400000},
	{"id": "gpt-5.5", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 400000},
	{"id": "gpt-5.4", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 400000},
	{"id": "gpt-5.3-codex", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 400000},
	{"id": "gemini-3.5-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	st.collector = h.cfg.Metrics
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	// Account pinning: /v1/a/<uid>/chat/completions forces every request to
	// that one account so an upstream gateway can round-robin across its own
	// per-account connections instead of racing the shared pool Pick().
	if pinUID := r.PathValue("uid"); pinUID != "" {
		stickyUID = pinUID
	}
	if stickyUID == "" && h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.unbindSession(sessKey)
			stickyUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				h.unbindSession(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			// 模型感知选号：6004 模型级冷却中的账号，换个模型仍可被选中
			// （模型限流不代表账号在其他模型下不可用）。
			acct = h.cfg.Pool.PickExcludingForModel(tried, peek.Model)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.unbindSession(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), peek.Model)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" {
			h.bindSession(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.usage = stats.Usage()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		if u, ok := resp["usage"].(map[string]any); ok {
			st.usage = ParseUsage(u)
		}
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// respBody 为上游原始响应体（用于识别 429 code=6004 的模型级限流重置时间），
// reqModel 为本次请求的模型名（用于记录触发模型，供后续豁免）。两者为空时
// 行为与旧版完全一致。
//
// 各路径职责：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 429 code=6004 且能解析「将在 … 重置」→ CooldownSoftForModel
//     （冷却截止取上游重置时间、记录触发模型以便换模型豁免）；否则退回
//     Cooldown(CoolSoft, soft_rate) 的基数 + 指数退避。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrBadParams → 不罚账号，仍轮转（网关侧/客户端 body 问题，与账号健康无关）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, respBody, reqModel string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 模型级限流优先：429 code=6004 明说「将在 YYYY-MM-DD HH:MM:SS UTC+8 重置」时，
		// 用上游给定的重置时刻作为冷却截止（而非固定基数+指数退避猜测），并记录触发
		// 模型——之后同账号换模型请求可被豁免（模型限流不代表账号整体不可用）。
		if resetAt, ok := upstream.ParseSoftRateReset(respBody); ok {
			h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, reqModel, "429 model rate limit")
			return
		}
		// 普通账号级软冷却：基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		// 连续 N 次才判死（12153 会被临时性触发，一次即禁用会误杀健康账号）。
		// 达阈值时 NoteSessionDead 内部完成 Disable。chat 路径与 keepalive 共用该计数。
		h.cfg.Pool.NoteSessionDead(uid)
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError）；但**仍然轮转**（不同账号可能有不同的模型
		// 权限，值得换号再试一次）。此处显式列出而非落 default，是为了让语义自解释。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
