package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// bonusStub 模拟补签卡 + 礼包/补偿端点（growth 域走 chatBase、billing 域走 billingBase）。
type bonusStub struct {
	heatErr     bool // heatmap 返回 500
	makeupErr   bool // makeup-cards/use 返回业务错误（无卡/无漏签）
	giftCredit  int64
	compCredit  int64
	heatCalls   atomic.Int32
	makeupBody  atomic.Value
	giftCalls   atomic.Int32
	compCalls   atomic.Int32
	// makeupTarget 传入的 target_date（断言用）。
}

func newBonusStub() *bonusStub {
	s := &bonusStub{giftCredit: 66, compCredit: 88}
	s.makeupBody.Store("")
	return s
}

func (s *bonusStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/heatmap":
			s.heatCalls.Add(1)
			if s.heatErr {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			// 昨日一格 score=0（漏签）+ 今日一格 score=1。
			yest := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
			today := time.Now().Format("2006-01-02")
			fmt.Fprintf(w, `{"code":0,"data":{"cells":[{"date":"%sT00:00:00+08:00","score":0},{"date":"%s","score":1}]}}`, yest, today)
		case "/activity/growth/streak":
			// makeup_cards 段（补签卡余额 2）。
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":5},"makeup_cards":{"balance":2,"max":5}}}`))
		case "/activity/growth/makeup-cards/use":
			var b struct {
				Date string `json:"target_date"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			s.makeupBody.Store(b.Date)
			if s.makeupErr {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"no makeup card available"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/billing/meter/claim-gift":
			s.giftCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"credit":%d}}`, s.giftCredit)
		case "/billing/meter/claim-compensation":
			s.compCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"credit":%d}}`, s.compCredit)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// TestGrowthStreakWithCards 解析 streak 响应的 makeup_cards 段（与 GrowthRewardState 同响应体）。
func TestGrowthStreakWithCards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/streak" {
			http.Error(w, "not found", 404)
			return
		}
		w.Write([]byte(`{"code":0,"data":{"streak":{"days":5},"makeup_cards":{"balance":2,"max":5}}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	st, err := c.GrowthStreakWithCards(a)
	if err != nil {
		t.Fatalf("GrowthStreakWithCards: %v", err)
	}
	if st.Streak.Days != 5 || st.MakeupCards.Balance != 2 || st.MakeupCards.Max != 5 {
		t.Errorf("days=%d balance=%d max=%d want 5/2/5", st.Streak.Days, st.MakeupCards.Balance, st.MakeupCards.Max)
	}
}

// TestGrowthHeatmapYesterdayMissed 昨日格 score==0 判漏签；date 带 T 时段也命中（前 10 位比较）。
func TestGrowthHeatmapYesterdayMissed(t *testing.T) {
	stub := newBonusStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	cells, err := c.GrowthHeatmap(a)
	if err != nil {
		t.Fatalf("GrowthHeatmap: %v", err)
	}
	score, ok := HeatmapDayScore(cells, GrowthYesterdayDate(time.Now()))
	if !ok || score != 0 {
		t.Errorf("yesterday score=%d ok=%v want 0/true（漏签判据）", score, ok)
	}
	// 今日有分。
	today, ok2 := HeatmapDayScore(cells, time.Now().In(cstShanghai).Format("2006-01-02"))
	if !ok2 || today != 1 {
		t.Errorf("today score=%d ok=%v want 1/true", today, ok2)
	}
}

// TestHeatmapDayScoreMissing 无该日格返回 (0, false)。
func TestHeatmapDayScoreMissing(t *testing.T) {
	cells := []HeatmapCell{{Date: "2026-09-15", Score: 1}}
	if _, ok := HeatmapDayScore(cells, "2026-01-01"); ok {
		t.Error("无该日格应返回 ok=false")
	}
}

// TestUseMakeupCard 请求体带 target_date（补签日期）。
func TestUseMakeupCard(t *testing.T) {
	stub := newBonusStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	if err := c.UseMakeupCard(a, "2026-09-15"); err != nil {
		t.Fatalf("UseMakeupCard: %v", err)
	}
	if got := stub.makeupBody.Load().(string); got != "2026-09-15" {
		t.Errorf("target_date=%q want 2026-09-15", got)
	}
}

// TestUseMakeupCardBusinessError 无卡 → 业务错误透传（调用方静默跳过）。
func TestUseMakeupCardBusinessError(t *testing.T) {
	stub := newBonusStub()
	stub.makeupErr = true
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	if err := c.UseMakeupCard(a, "2026-09-15"); err == nil {
		t.Fatal("无卡应返回业务错误")
	}
}

// TestClaimGiftAndCompensation 礼包/补偿领取：billing 域 POST → data.credit。
func TestClaimGiftAndCompensation(t *testing.T) {
	stub := newBonusStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	credit, err := c.ClaimGift(a)
	if err != nil || credit != 66 {
		t.Errorf("ClaimGift credit=%d err=%v want 66/nil", credit, err)
	}
	credit, err = c.ClaimCompensation(a)
	if err != nil || credit != 88 {
		t.Errorf("ClaimCompensation credit=%d err=%v want 88/nil", credit, err)
	}
	if n := stub.giftCalls.Load(); n != 1 {
		t.Errorf("gift calls=%d want 1", n)
	}
	if n := stub.compCalls.Load(); n != 1 {
		t.Errorf("comp calls=%d want 1", n)
	}
}

// TestClaimGiftBusinessError 已领（业务错误）→ 透传 err，credit=0。
func TestClaimGiftBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"code":400,"msg":"gift already claimed"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	credit, err := c.ClaimGift(a)
	if err == nil || credit != 0 {
		t.Errorf("已领应返回 err（credit=%d）", credit)
	}
}

// TestGrowthYesterdayDateCST 昨日日期按 CST 自然日（跨 UTC 日边界稳定）。
func TestGrowthYesterdayDateCST(t *testing.T) {
	// 2026-09-16 01:00 CST（前一日 UTC 2026-09-15 17:00）：昨日 CST = 09-15。
	now := time.Date(2026, 9, 16, 1, 0, 0, 0, cstShanghai)
	if got := GrowthYesterdayDate(now); got != "2026-09-15" {
		t.Errorf("GrowthYesterdayDate=%q want 2026-09-15", got)
	}
}
