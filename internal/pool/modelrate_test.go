package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 说明：模型级豁免只作用于 normal 选号。当**全部**账号都在冷却时，池子会走
// 全冷却兜底（pickEarliestExpiryLocked）挑"最早到期"的账号顶班——那是"无任何可用"
// 时的降级，优先保可用性而非严格豁免。因此下面用双账号构造，隔离 normal 路径。

// TestCooldownSoftForModelUsesResetTime 6004 带重置时间时，冷却截止应取上游给定时刻
// （而非固定基数+指数退避），并记录触发模型。
func TestCooldownSoftForModelUsesResetTime(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	resetAt := time.Now().Add(90 * time.Second)
	p.CooldownSoftForModel("u1", 10*time.Minute, resetAt, "glm-5.2", "429 model rate limit")

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("账号应处于冷却中")
	}
	// 剩余时间应接近 resetAt（90s），而不是基数 10min。
	if st.CoolRemaining > 95 || st.CoolRemaining < 80 {
		t.Errorf("冷却剩余 = %ds, 应接近上游重置时间 90s（而非基数 600s）", st.CoolRemaining)
	}
	if model, isModel := p.ModelSoftCooldown("u1"); !isModel || model != "glm-5.2" {
		t.Errorf("应记录触发模型 glm-5.2, 得到 %q (isModel=%v)", model, isModel)
	}
}

// TestCooldownSoftForModelCapsAtSoftRateMax 上游重置时间远超 soft_rate_max 时应被封顶，
// 避免一条异常文案把账号冷却到很远的未来。
func TestCooldownSoftForModelCapsAtSoftRateMax(t *testing.T) {
	p := New("")
	p.SetSoftRateMax(5 * time.Minute)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	// 上游说 10 小时后重置，但封顶 5 分钟。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Hour), "glm-5.2", "x")

	st, _ := p.Status("u1")
	if st.CoolRemaining > 5*60+2 {
		t.Errorf("冷却剩余 = %ds, 应被 soft_rate_max(300s) 封顶", st.CoolRemaining)
	}
}

// TestCooldownSoftForModelNoResetFallsBack 无重置时间时完全退回既有行为
// （基数 + 指数退避），且不记录模型（不产生豁免）。
func TestCooldownSoftForModelNoResetFallsBack(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	p.CooldownSoftForModel("u1", 10*time.Minute, time.Time{}, "", "429 rate limit")

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("账号应处于冷却中")
	}
	if st.CoolRemaining > 601 || st.CoolRemaining < 595 {
		t.Errorf("冷却剩余 = %ds, 应约等于基数 600s", st.CoolRemaining)
	}
	if model, isModel := p.ModelSoftCooldown("u1"); isModel {
		t.Errorf("无重置时间不应记录模型（不应产生豁免）, 得到 %q", model)
	}
}

// TestModelExemptionDifferentModelUsable 模型级冷却中的账号换个模型仍可被选中，
// 而同模型不会被选中（冷却对该模型仍然生效）。
func TestModelExemptionDifferentModelUsable(t *testing.T) {
	p := New("")
	// u1 对 glm-5.2 模型级冷却；u2 完全健康（提供 normal 选号的对照）。
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 model rate limit")

	// 同模型请求：u1 被冷却（不豁免），只能选到 u2。
	got := p.PickExcludingForModel(nil, "glm-5.2")
	if got == nil {
		t.Fatal("应能选中 u2")
	}
	if got.UID == "u1" {
		t.Errorf("同模型下 u1 处于冷却，不应被选中")
	}

	// 不同模型请求：u1 被豁免，进入候选（此处只断言"不再是不可用"：
	// 用只含 u1 的池验证豁免确实放行）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p2.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 model rate limit")
	if got := p2.PickExcludingForModel(nil, "deepseek-v4.1-flash"); got == nil {
		t.Error("换模型应可被选中（模型级豁免）")
	} else if got.UID != "u1" {
		t.Errorf("选中了 %s, want u1", got.UID)
	}
	// 空模型：不豁免（保持既有语义）→ 走全冷却兜底仍会返回账号，
	// 故直接断言 healthyForModel 口径。
	p2.mu.RLock()
	e := p2.byUID["u1"]
	emptyModelExempt := e.healthyForModel(time.Now(), "")
	p2.mu.RUnlock()
	if emptyModelExempt {
		t.Error("空模型不应豁免")
	}
}

// TestModelExemptionNotAppliedToAccountLevelCooldown 普通账号级软冷却不留豁免痕迹：
// 换模型也必须仍被冷却（否则会错误绕过账号级限流）。
func TestModelExemptionNotAppliedToAccountLevelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	// 先来一次模型级冷却（留下 softRateModel）……
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "model")
	// ……随后一次账号级冷却应清空豁免痕迹。
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")

	if model, isModel := p.ModelSoftCooldown("u1"); isModel {
		t.Errorf("账号级冷却应清空模型豁免痕迹, 得到 %q", model)
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	exempt := e.healthyForModel(time.Now(), "deepseek-v4.1-flash")
	p.mu.RUnlock()
	if exempt {
		t.Error("账号级冷却下换模型不应豁免")
	}
}

// TestModelExemptionRespectsDisabledAndBreaker 模型豁免不得越过禁用与熔断
// （那是账号级事实）。
func TestModelExemptionRespectsDisabledAndBreaker(t *testing.T) {
	t.Run("禁用", func(t *testing.T) {
		p := New("")
		p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
		p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "model")
		p.Disable("u1", "manual")
		p.mu.RLock()
		e := p.byUID["u1"]
		exempt := e.healthyForModel(time.Now(), "other-model")
		p.mu.RUnlock()
		if exempt {
			t.Error("禁用账号不应因模型豁免被放行")
		}
	})

	t.Run("熔断", func(t *testing.T) {
		p := New("")
		p.SetBreaker(1, time.Hour, time.Hour) // 一次失败即熔断
		p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
		p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "model")
		// 熔断已由上面的冷却入口喂入（threshold=1 立即生效）。
		p.mu.RLock()
		e := p.byUID["u1"]
		exempt := e.healthyForModel(time.Now(), "other-model")
		p.mu.RUnlock()
		if exempt {
			t.Error("熔断中的账号不应因模型豁免被放行")
		}
	})
}

// TestModelExemptionRevivedBySignin 签到解冻清冷却时应一并清空模型豁免痕迹。
func TestModelExemptionRevivedBySignin(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "model")

	p.ReenableIfCredits("u1", 100) // 签到解冻

	if model, isModel := p.ModelSoftCooldown("u1"); isModel {
		t.Errorf("解冻后不应残留模型豁免痕迹, 得到 %q", model)
	}
}

// TestModelExemptionExpiredBreakerStillUsable 已过期的熔断不应阻止模型豁免
// （回归防护：不能误用 breakerUntil.IsZero() 判断，过期时间戳仍非零）。
func TestModelExemptionExpiredBreakerStillUsable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.until = time.Now().Add(time.Hour)
	e.coolKind = CoolSoft
	e.softRateModel = "glm-5.2"
	e.breakerUntil = time.Now().Add(-time.Hour) // 熔断已过期
	p.mu.Unlock()

	p.mu.RLock()
	exempt := e.healthyForModel(time.Now(), "other-model")
	p.mu.RUnlock()
	if !exempt {
		t.Error("熔断已过期时，模型豁免应放行（不能把非零时间戳当作仍生效）")
	}
}

// TestModelSoftCooldownUnknownUID 未知 uid 不 panic。
func TestModelSoftCooldownUnknownUID(t *testing.T) {
	p := New("")
	if model, ok := p.ModelSoftCooldown("nope"); ok || model != "" {
		t.Errorf("未知 uid 应返回空, 得到 %q/%v", model, ok)
	}
}
