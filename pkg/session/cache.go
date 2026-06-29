package session

import (
	"sync"
	"time"
)

const defaultCacheTTL = 30 * time.Second

type podEntry struct {
	podIP     string
	podName   string
	expiresAt time.Time
}

// PodCache is a thread-safe in-memory cache mapping session IDs to pod
// IPs and names with per-entry TTL expiry.
type PodCache struct {
	mu      sync.RWMutex
	entries map[string]*podEntry
	ttl     time.Duration
}

// NewPodCache creates a PodCache with the given TTL. If ttl <= 0 the
// default of 30 seconds is used.
func NewPodCache(ttl time.Duration) *PodCache {
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return &PodCache{
		entries: make(map[string]*podEntry),
		ttl:     ttl,
	}
}

// Get returns the cached pod IP and name for the session. Returns empty
// strings and false on miss or TTL expiry.
func (c *PodCache) Get(sessionID string) (podIP, podName string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, exists := c.entries[sessionID]
	if !exists || time.Now().After(e.expiresAt) {
		return "", "", false
	}
	return e.podIP, e.podName, true
}

// Set stores a pod IP and name with a fresh TTL, overwriting any existing entry.
func (c *PodCache) Set(sessionID, podIP, podName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[sessionID] = &podEntry{
		podIP:     podIP,
		podName:   podName,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Delete removes the cache entry for the given session unconditionally.
func (c *PodCache) Delete(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sessionID)
}

// Invalidate removes the entry only if the stored pod IP matches.
// This prevents evicting a freshly-updated entry after reconnect.
func (c *PodCache) Invalidate(sessionID, podIP string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[sessionID]; ok && e.podIP == podIP {
		delete(c.entries, sessionID)
	}
}

// EvictExpired removes all entries whose TTL has elapsed and returns the count.
func (c *PodCache) EvictExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	evicted := 0
	for id, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, id)
			evicted++
		}
	}
	return evicted
}

// Len returns the number of entries (including expired but not yet evicted).
func (c *PodCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
