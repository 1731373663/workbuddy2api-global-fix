package upstream

import (
	"testing"
	"time"
)

// TestIsModelRateLimit 模型级限流识别（code 6004），含 JSON 空格容差。
func TestIsModelRateLimit(t *testing.T) {
	yes := []string{
		`{"code":6004,"msg":"您的使用量已超出频率限制"}`,
		`{"code": 6004,"msg":"x"}`,
		`{"code":"6004","msg":"x"}`,
		`{"code":"6004"}`,
	}
	for _, b := range yes {
		if !IsModelRateLimit(b) {
			t.Errorf("应识别为模型级限流: %s", b)
		}
	}
	no := []string{
		`{"code":11140,"msg":"rate-limiting"}`, // 通用限流，非模型级
		`{"code":6005}`,
		`rate limit`,
		``,
	}
	for _, b := range no {
		if IsModelRateLimit(b) {
			t.Errorf("不应识别为模型级限流: %s", b)
		}
	}
}

// TestParseSoftRateReset 从 6004 body 解析「将在 … 重置」的墙钟时间（UTC+8）。
func TestParseSoftRateReset(t *testing.T) {
	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-12 13:56:36 UTC+8 重置，您也可以切换其他模型继续使用。"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatal("应能解析出重置时间")
	}
	// 按 UTC+8 解释：2026-09-12 13:56:36 +0800。
	want := time.Date(2026, 9, 12, 13, 56, 36, 0, SoftRateResetLoc())
	if !got.Equal(want) {
		t.Errorf("解析结果 = %v, want %v", got, want)
	}
}

// TestParseSoftRateResetRejectsNonModelLimit 非模型级限流即使带"重置"字样也不解析
// ——该重置无冷却语义，解析出来会错误收窄冷却。
func TestParseSoftRateResetRejectsNonModelLimit(t *testing.T) {
	cases := []string{
		`{"code":11140,"msg":"将在 2026-09-12 13:56:36 UTC+8 重置"}`, // 非 6004
		`{"code":6004,"msg":"没有重置字样"}`,                           // 6004 但无时间
		`{"code":6004,"msg":"将在 不是时间 重置"}`,                       // 格式非法
		``,
	}
	for _, b := range cases {
		if _, ok := ParseSoftRateReset(b); ok {
			t.Errorf("不应解析成功: %s", b)
		}
	}
}

// TestParseSoftRateResetTimezoneIsUTC8 时区必须固定 UTC+8（不受进程 TZ 影响）。
func TestParseSoftRateResetTimezoneIsUTC8(t *testing.T) {
	body := `{"code":6004,"msg":"将在 2026-01-02 03:04:05 UTC+8 重置"}`
	got, ok := ParseSoftRateReset(body)
	if !ok {
		t.Fatal("解析失败")
	}
	_, offset := got.Zone()
	if offset != 8*60*60 {
		t.Errorf("时区偏移 = %d, want %d（UTC+8）", offset, 8*60*60)
	}
}
