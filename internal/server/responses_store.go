// responses_store.go 本地 Responses 结果存储。
//
// 上游只讲 Chat Completions，没有 Responses 服务端状态。为了让
// previous_response_id / store / GET /v1/responses/{id} 具备可用的本地语义，
// 网关在进程内保存每次 store=true 的结果：完整 Response 对象（供 GET 回读）
// 与本次出站的 Chat messages + assistant 回复（供下一轮拼接历史）。
//
// 刻意只放内存：不落盘、不增长磁盘、不引入新配置项。上限与 TTL 双重约束，
// 超限按插入顺序淘汰最旧条目，过期条目惰性清理。进程重启后历史清空——
// 这是可接受的降级：客户端会收到 404 并回到「重发完整历史」的既有路径。
package server

import (
	"sync"
	"time"
)

const (
	// responsesStoreMaxEntries 单进程最多保存的响应条数。
	responsesStoreMaxEntries = 512
	// responsesStoreTTL 条目存活时长；到期后 GET/previous_response_id 视为不存在。
	responsesStoreTTL = 24 * time.Hour
)

// responsesEntry 一条已保存的响应。
type responsesEntry struct {
	envelope  map[string]any // Response 对象（GET 回读）
	messages  []any          // 出站 Chat messages + assistant 回复（下一轮拼接）
	createdAt time.Time
	expiresAt time.Time
}

// responsesStore 有界 TTL 的响应存储，并发安全。
type responsesStore struct {
	mu      sync.Mutex
	entries map[string]*responsesEntry
	order   []string // 插入顺序，用于淘汰最旧条目
	max     int
	ttl     time.Duration
}

func newResponsesStore(max int, ttl time.Duration) *responsesStore {
	if max <= 0 {
		max = responsesStoreMaxEntries
	}
	if ttl <= 0 {
		ttl = responsesStoreTTL
	}
	return &responsesStore{entries: map[string]*responsesEntry{}, max: max, ttl: ttl}
}

// Put 保存响应；同 id 覆盖。超限时淘汰最旧条目。
func (s *responsesStore) Put(id string, envelope map[string]any, messages []any) {
	if s == nil || id == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	if _, exists := s.entries[id]; !exists {
		s.order = append(s.order, id)
	}
	s.entries[id] = &responsesEntry{
		envelope: envelope, messages: messages,
		createdAt: now, expiresAt: now.Add(s.ttl),
	}
	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// Get 读取未过期条目；过期或不存在返回 ok=false。
func (s *responsesStore) Get(id string) (map[string]any, []any, bool) {
	if s == nil || id == "" {
		return nil, nil, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	e, ok := s.entries[id]
	if !ok {
		return nil, nil, false
	}
	return e.envelope, e.messages, true
}

// Delete 删除条目；返回是否命中。
func (s *responsesStore) Delete(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return false
	}
	delete(s.entries, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

// Len 当前未过期条目数（观测/测试用）。
func (s *responsesStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
	return len(s.entries)
}

// gcLocked 清理过期条目；调用方须持锁。
func (s *responsesStore) gcLocked(now time.Time) {
	if len(s.entries) == 0 {
		return
	}
	alive := s.order[:0]
	for _, id := range s.order {
		e, ok := s.entries[id]
		if !ok {
			continue
		}
		if now.After(e.expiresAt) {
			delete(s.entries, id)
			continue
		}
		alive = append(alive, id)
	}
	s.order = alive
}
