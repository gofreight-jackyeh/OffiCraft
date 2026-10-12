package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// seenStore is a dedup table persisted as JSON: key -> first-seen time in
// fractional unix seconds. It survives restarts on purpose — a reconnect right
// after a restart is exactly when Slack re-delivers. Entries older than keep
// are pruned on every write (slack 7 days, github 180 days).
//
// Deliberate ceiling: the whole map is rewritten on each claim. At a few
// hundred messages a day that is a few KB; if traffic ever reaches thousands
// per minute, move to an append-only log.
type seenStore struct {
	path string
	keep time.Duration

	mu      sync.Mutex
	m       map[string]float64
	existed bool // the file was present when opened (github-watch seeds otherwise)
}

func openSeen(path string, keep time.Duration) (*seenStore, error) {
	s := &seenStore{path: path, keep: keep, m: map[string]float64{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.existed = true
	if err := json.Unmarshal(data, &s.m); err != nil {
		return nil, fmt.Errorf("dedup file %s is corrupt: %w", path, err)
	}
	if s.m == nil {
		s.m = map[string]float64{}
	}
	return s, nil
}

// add records key in memory; false when it was already there (a re-delivery).
func (s *seenStore) add(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		return false
	}
	s.m[key] = float64(now.UnixNano()) / 1e9
	return true
}

// save prunes expired keys and writes the file atomically.
func (s *seenStore) save(now time.Time) error {
	// Held through the write so two concurrent saves cannot land out of order.
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := float64(now.Add(-s.keep).UnixNano()) / 1e9
	for k, ts := range s.m {
		if ts < cutoff {
			delete(s.m, k)
		}
	}
	data, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}

// claim = add + save. The bool is the dedup verdict; a save error is reported
// separately so the caller can still act on the in-memory verdict.
func (s *seenStore) claim(key string, now time.Time) (bool, error) {
	if !s.add(key, now) {
		return false, nil
	}
	return true, s.save(now)
}
