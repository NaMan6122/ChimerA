package webapi

import "sync"

// DefaultMaxConversations caps in-memory thread->upstream conversation tables
// across browserless providers so ephemeral X-Session-Id values cannot grow
// memory without bound.
const DefaultMaxConversations = 512

// ConvStore is a concurrency-safe bounded map with FIFO oldest-entry eviction.
// A zero-value ConvStore is ready to use and defaults to DefaultMaxConversations.
type ConvStore[V any] struct {
	mu    sync.Mutex
	max   int
	items map[string]V
	order []string
}

// NewConvStore builds a ConvStore with the given maximum capacity (<=0 uses
// DefaultMaxConversations).
func NewConvStore[V any](max int) *ConvStore[V] {
	if max <= 0 {
		max = DefaultMaxConversations
	}
	return &ConvStore[V]{
		max:   max,
		items: make(map[string]V),
	}
}

func (s *ConvStore[V]) ensureInit() {
	if s.max <= 0 {
		s.max = DefaultMaxConversations
	}
	if s.items == nil {
		s.items = make(map[string]V)
	}
}

// Get returns the value mapped to key, if present.
func (s *ConvStore[V]) Get(key string) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		var zero V
		return zero, false
	}
	v, ok := s.items[key]
	return v, ok
}

// Set inserts or updates key. When inserting a new key at capacity, the oldest
// entry is evicted so active sibling sessions stay warm.
func (s *ConvStore[V]) Set(key string, val V) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureInit()
	if _, exists := s.items[key]; exists {
		s.items[key] = val
		return
	}
	for len(s.items) >= s.max && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.items, oldest)
	}
	s.items[key] = val
	s.order = append(s.order, key)
}

// Delete removes key if present.
func (s *ConvStore[V]) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		return
	}
	if _, exists := s.items[key]; !exists {
		return
	}
	delete(s.items, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Clear drops every stored conversation mapping.
func (s *ConvStore[V]) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]V)
	s.order = nil
}

// Len returns the number of active mappings.
func (s *ConvStore[V]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}
