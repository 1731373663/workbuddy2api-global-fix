package server

import (
	"testing"
	"time"
)

// TestResponsesStorePutGetDelete covers the basic local response lifetime.
func TestResponsesStorePutGetDelete(t *testing.T) {
	s := newResponsesStore(2, time.Hour)
	env := map[string]any{"id": "resp_1", "status": "completed"}
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	s.Put("resp_1", env, msgs)

	gotEnv, gotMsgs, ok := s.Get("resp_1")
	if !ok || gotEnv["id"] != "resp_1" || len(gotMsgs) != 1 {
		t.Fatalf("Get resp_1 = %v %v %v", gotEnv, gotMsgs, ok)
	}
	if !s.Delete("resp_1") {
		t.Fatal("Delete resp_1 should return true")
	}
	if _, _, ok := s.Get("resp_1"); ok {
		t.Fatal("resp_1 should be gone after Delete")
	}
	if s.Delete("resp_1") {
		t.Fatal("second Delete should return false")
	}
}

// TestResponsesStoreEvictsOldest keeps the store bounded.
func TestResponsesStoreEvictsOldest(t *testing.T) {
	s := newResponsesStore(2, time.Hour)
	s.Put("a", map[string]any{"id": "a"}, nil)
	s.Put("b", map[string]any{"id": "b"}, nil)
	s.Put("c", map[string]any{"id": "c"}, nil)
	if _, _, ok := s.Get("a"); ok {
		t.Fatal("oldest entry a should be evicted")
	}
	if _, _, ok := s.Get("b"); !ok {
		t.Fatal("b should remain")
	}
	if _, _, ok := s.Get("c"); !ok {
		t.Fatal("c should remain")
	}
}

// TestResponsesStoreTTL drops expired entries.
func TestResponsesStoreTTL(t *testing.T) {
	s := newResponsesStore(2, time.Nanosecond)
	s.Put("a", map[string]any{"id": "a"}, nil)
	time.Sleep(2 * time.Millisecond)
	if _, _, ok := s.Get("a"); ok {
		t.Fatal("expired entry should not be returned")
	}
}
