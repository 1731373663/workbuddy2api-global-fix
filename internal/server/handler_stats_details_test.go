package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/metrics"
)

// TestStatsDetailsEndpoint 详情接口按模型过滤并返回请求级字段。
func TestStatsDetailsEndpoint(t *testing.T) {
	m := metrics.New("")
	m.Record(metrics.Delta{Model: "a", OK: true, Status: 200, PromptTokens: 10, CompletionTokens: 2})
	m.Record(metrics.Delta{Model: "b", OK: true, Status: 200, PromptTokens: 20, CompletionTokens: 3})
	h := NewHandler(Config{Pool: testPoolWith(), Metrics: m})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/stats/details?model=a&limit=10", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Enabled bool                    `json:"enabled"`
		Count   int                     `json:"count"`
		Details []metrics.RequestDetail `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || out.Count != 1 || len(out.Details) != 1 || out.Details[0].Model != "a" {
		t.Fatalf("unexpected details response: %+v", out)
	}
}
