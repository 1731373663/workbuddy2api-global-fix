package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// newPoolWith 构造只含指定账号的池（session-dead 相关测试共用）。
func newPoolWith(uids ...string) *Pool {
	p := New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid})
	}
	return p
}

// TestNoteSessionDeadRequiresConsecutive 连续 12153 未达阈值时不得禁用账号。
//
// 背景：旧行为一次 12153 就 Disable 且无复活路径，但 12153 会被临时性触发
// （网络抖动/上游闪断/refresh 竞态），一次失败即永久杀号会误杀健康账号。
func TestNoteSessionDeadRequiresConsecutive(t *testing.T) {
	p := newPoolWith("u1")
	threshold := SessionDeadThreshold()

	// 前 threshold-1 次都不得禁用。
	for i := 1; i < threshold; i++ {
		if disabled := p.NoteSessionDead("u1"); disabled {
			t.Fatalf("第 %d 次不应禁用（阈值 %d）", i, threshold)
		}
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("第 %d 次后账号不应 disabled", i)
		}
	}
	// 第 threshold 次达阈值 → 禁用。
	if disabled := p.NoteSessionDead("u1"); !disabled {
		t.Fatalf("第 %d 次应触发禁用", threshold)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Error("达阈值后账号应 disabled")
	}
	if st.DisabledReason == "" {
		t.Error("禁用账号应透出 disabled_reason")
	}
}

// TestClearSessionDeadResetsCounter 成功（清计数）后，连续计数应从头开始，
// 不会因历史累积被追杀。
func TestClearSessionDeadResetsCounter(t *testing.T) {
	p := newPoolWith("u1")
	threshold := SessionDeadThreshold()

	// 攒到阈值-1 次。
	for i := 1; i < threshold; i++ {
		p.NoteSessionDead("u1")
	}
	// 一次成功清计数。
	p.ClearSessionDead("u1")

	// 再攒阈值-1 次仍不应禁用（证明计数真的清零了）。
	for i := 1; i < threshold; i++ {
		if disabled := p.NoteSessionDead("u1"); disabled {
			t.Fatalf("计数应已清零，第 %d 次不应禁用", i)
		}
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("清计数后不应 disabled")
	}
}

// TestNoteSuccessClearsSessionDead 聊天成功也应清连续 12153 计数。
func TestNoteSuccessClearsSessionDead(t *testing.T) {
	p := newPoolWith("u1")
	threshold := SessionDeadThreshold()

	for i := 1; i < threshold; i++ {
		p.NoteSessionDead("u1")
	}
	p.NoteSuccess("u1") // 成功证明 session 未死

	for i := 1; i < threshold; i++ {
		if disabled := p.NoteSessionDead("u1"); disabled {
			t.Fatalf("NoteSuccess 后计数应清零，第 %d 次不应禁用", i)
		}
	}
}

// TestReviveDisabled 复活入口：清除 disabled + reason + 计数，账号回到池子。
func TestReviveDisabled(t *testing.T) {
	p := newPoolWith("u1")
	threshold := SessionDeadThreshold()
	for i := 0; i < threshold; i++ {
		p.NoteSessionDead("u1")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("前置条件：账号应已禁用")
	}

	p.ReviveDisabled("u1")
	st, _ = p.Status("u1")
	if st.Disabled {
		t.Error("复活后不应 disabled")
	}
	if st.DisabledReason != "" {
		t.Errorf("复活后应清空 disabled_reason, 得到 %q", st.DisabledReason)
	}
	// 复活后账号应可被选中（healthy）。
	if p.Pick() == nil {
		t.Error("复活后账号应可被 Pick 选中")
	}
}

// TestReviveDisabledOnlyAffectsDisabled 复活只对 disabled 账号生效，
// 不影响正常账号的状态。
func TestReviveDisabledOnlyAffectsDisabled(t *testing.T) {
	p := newPoolWith("u1")
	p.NoteSuccess("u1")
	before, _ := p.Status("u1")

	p.ReviveDisabled("u1") // 对未禁用账号是空操作

	after, _ := p.Status("u1")
	if after.Disabled != before.Disabled || after.SuccessCount != before.SuccessCount {
		t.Errorf("复活不应改动未禁用账号: before=%+v after=%+v", before, after)
	}
}

// TestNoteSessionDeadUnknownUID 未知 uid 不应 panic。
func TestNoteSessionDeadUnknownUID(t *testing.T) {
	p := newPoolWith("u1")
	if p.NoteSessionDead("nope") {
		t.Error("未知 uid 不应返回 true")
	}
	p.ClearSessionDead("nope")
	p.ReviveDisabled("nope")
}
