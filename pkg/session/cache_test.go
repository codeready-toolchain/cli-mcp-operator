package session

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPodCache_SetAndGet(t *testing.T) {
	// given
	cache := NewPodCache(5 * time.Second)
	cache.Set("inv-1", "10.0.0.1", "pod-abc")

	// when
	ip, name, ok := cache.Get("inv-1")

	// then
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1", ip)
	assert.Equal(t, "pod-abc", name)
}

func TestPodCache_GetMiss(t *testing.T) {
	t.Run("unknown session returns miss", func(t *testing.T) {
		// given
		cache := NewPodCache(5 * time.Second)

		// when
		ip, name, ok := cache.Get("does-not-exist")

		// then
		assert.False(t, ok)
		assert.Empty(t, ip)
		assert.Empty(t, name)
	})

	t.Run("expired entry returns miss", func(t *testing.T) {
		// given
		cache := NewPodCache(1 * time.Millisecond)
		cache.Set("inv-1", "10.0.0.1", "pod-abc")
		time.Sleep(5 * time.Millisecond)

		// when
		ip, name, ok := cache.Get("inv-1")

		// then
		assert.False(t, ok)
		assert.Empty(t, ip)
		assert.Empty(t, name)
	})
}

func TestPodCache_Delete(t *testing.T) {
	// given
	cache := NewPodCache(5 * time.Second)
	cache.Set("inv-1", "10.0.0.1", "pod-abc")

	// when
	cache.Delete("inv-1")
	_, _, ok := cache.Get("inv-1")

	// then
	assert.False(t, ok)
}

func TestPodCache_Invalidate(t *testing.T) {
	t.Run("removes entry when pod IP matches", func(t *testing.T) {
		// given
		cache := NewPodCache(5 * time.Second)
		cache.Set("inv-1", "10.0.0.1", "pod-abc")

		// when
		cache.Invalidate("inv-1", "10.0.0.1")
		_, _, ok := cache.Get("inv-1")

		// then
		assert.False(t, ok)
	})

	t.Run("preserves entry when pod IP differs", func(t *testing.T) {
		// given
		cache := NewPodCache(5 * time.Second)
		cache.Set("inv-1", "10.0.0.1", "pod-abc")

		// when
		cache.Invalidate("inv-1", "10.0.0.99")
		ip, name, ok := cache.Get("inv-1")

		// then
		require.True(t, ok)
		assert.Equal(t, "10.0.0.1", ip)
		assert.Equal(t, "pod-abc", name)
	})
}

func TestPodCache_EvictExpired(t *testing.T) {
	// given
	cache := NewPodCache(1 * time.Millisecond)
	cache.Set("expired-1", "10.0.0.1", "pod-1")
	cache.Set("expired-2", "10.0.0.2", "pod-2")
	time.Sleep(5 * time.Millisecond)
	cache.Set("fresh", "10.0.0.3", "pod-3")

	// when
	evicted := cache.EvictExpired()

	// then
	assert.Equal(t, 2, evicted)
	assert.Equal(t, 1, cache.Len())
	_, _, ok := cache.Get("fresh")
	assert.True(t, ok)
}

func TestPodCache_DefaultTTL(t *testing.T) {
	tests := []struct {
		name string
		ttl  time.Duration
	}{
		{"zero uses default", 0},
		{"negative uses default", -1 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// when
			cache := NewPodCache(tt.ttl)

			// then
			assert.Equal(t, defaultCacheTTL, cache.ttl)
		})
	}
}

func TestPodCache_SetOverwritesExisting(t *testing.T) {
	// given
	cache := NewPodCache(5 * time.Second)
	cache.Set("inv-1", "10.0.0.1", "pod-old")

	// when
	cache.Set("inv-1", "10.0.0.99", "pod-new")
	ip, name, ok := cache.Get("inv-1")

	// then
	require.True(t, ok)
	assert.Equal(t, "10.0.0.99", ip)
	assert.Equal(t, "pod-new", name)
}

func TestPodCache_ConcurrentAccess(t *testing.T) {
	// given
	cache := NewPodCache(5 * time.Second)
	var wg sync.WaitGroup

	// when — hammer the cache from multiple goroutines
	for range 100 {
		wg.Add(4)
		go func() {
			defer wg.Done()
			cache.Set("inv-1", "10.0.0.1", "pod-1")
		}()
		go func() {
			defer wg.Done()
			cache.Get("inv-1")
		}()
		go func() {
			defer wg.Done()
			cache.Delete("inv-1")
		}()
		go func() {
			defer wg.Done()
			cache.Invalidate("inv-1", "10.0.0.1")
		}()
	}
	wg.Wait()

	// then — no data race (test passes with -race)
}

func TestPodCache_Len(t *testing.T) {
	// given
	cache := NewPodCache(5 * time.Second)
	assert.Equal(t, 0, cache.Len())

	// when
	cache.Set("a", "1.1.1.1", "pod-a")
	cache.Set("b", "2.2.2.2", "pod-b")

	// then
	assert.Equal(t, 2, cache.Len())

	cache.Delete("a")
	assert.Equal(t, 1, cache.Len())
}
