package main

import (
	"encoding/json"
	"hash/fnv"
	"sync"
	"time"
)

// The protocols carry no session identifier, so a conversation is identified
// by a hash of the model plus the FIRST message of the request (the system
// prompt for coding agents): only messages[0] is guaranteed identical across
// turns of one conversation, while a longer prefix would rehash on the early
// turns as the history grows. It is a heuristic — a miss only degrades to
// round-robin.
const (
	affinityWindow   = 1
	affinityMaxBytes = 8 << 10
	stickyTTL        = 30 * time.Minute
	stickyMaxEntries = 10000
)

// stickyHeader: stickyOff routes a request round-robin, unbound. A batch
// client sends it when many independent requests share one system prompt:
// they would all hash to one conversation and pile onto one key's rate limit.
const (
	stickyHeader = "X-Ferry-Sticky"
	stickyOff    = "off"
)

// stickySessions binds conversation hashes to upstream keys, so a
// multi-account setup keeps one conversation on one account and its
// prompt-cache hits.
type stickySessions struct {
	mu   sync.Mutex
	bind map[uint64]stickyBinding
}

type stickyBinding struct {
	key      int
	lastUsed time.Time
}

func (s *stickySessions) get(id uint64, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bind[id]
	if !ok {
		return -1
	}
	if now.Sub(b.lastUsed) > stickyTTL {
		delete(s.bind, id)
		return -1
	}
	b.lastUsed = now
	s.bind[id] = b
	return b.key
}

// reset drops every binding: Reload shifts key indexes, so old bindings
// would point at the wrong key. Conversations rebind on their next request.
func (s *stickySessions) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bind = nil
}

func (s *stickySessions) put(id uint64, key int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bind == nil {
		s.bind = make(map[uint64]stickyBinding)
	}
	if len(s.bind) >= stickyMaxEntries {
		for k, b := range s.bind {
			if now.Sub(b.lastUsed) > stickyTTL {
				delete(s.bind, k)
			}
		}
		// Still full: skip this binding; the conversation keeps round-robin
		// until expired entries free space.
		if len(s.bind) >= stickyMaxEntries {
			return
		}
	}
	s.bind[id] = stickyBinding{key: key, lastUsed: now}
}

// affinityID returns 0 when the body holds nothing stable to hash (then the
// request is routed round-robin).
func affinityID(obj map[string]json.RawMessage, model string) uint64 {
	if obj == nil {
		return 0
	}
	var raw json.RawMessage
	switch {
	case obj["messages"] != nil:
		raw = obj["messages"]
	case obj["input"] != nil:
		raw = obj["input"]
	default:
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(model))
	h.Write([]byte{0})
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err == nil {
		for i, elem := range elems {
			if i >= affinityWindow {
				break
			}
			h.Write(elem)
		}
	} else {
		b := raw
		if len(b) > affinityMaxBytes {
			b = b[:affinityMaxBytes]
		}
		h.Write(b)
	}
	return h.Sum64()
}
