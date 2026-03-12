package adapterproxy

import (
	"sync"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

// adapterInfoCache provides identity caching for INFO IDs 0x00, 0x01, 0x02.
// Telemetry IDs (0x03–0x07) are always forwarded upstream.
type adapterInfoCache struct {
	mu      sync.Mutex
	entries map[byte]*infoCacheEntry
}

type infoCacheEntry struct {
	frames    []downstream.Frame // complete response frame sequence (length + data frames)
	updatedAt time.Time
}

func newAdapterInfoCache() *adapterInfoCache {
	return &adapterInfoCache{
		entries: make(map[byte]*infoCacheEntry),
	}
}

// isIdentityID returns true for INFO IDs that should be cached (static per session).
func isIdentityID(infoID byte) bool {
	return infoID <= 0x02 // 0x00=Version, 0x01=HW ID, 0x02=HW Config
}

// get returns cached frames for an identity INFO ID, or nil if not cached.
func (c *adapterInfoCache) get(infoID byte) []downstream.Frame {
	if !isIdentityID(infoID) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[infoID]
	if !ok {
		return nil
	}
	// Return a copy to avoid mutation.
	frames := make([]downstream.Frame, len(entry.frames))
	for i, f := range entry.frames {
		payload := make([]byte, len(f.Payload))
		copy(payload, f.Payload)
		frames[i] = downstream.Frame{Command: f.Command, Payload: payload}
	}
	return frames
}

// put stores the complete frame sequence for an identity INFO ID.
func (c *adapterInfoCache) put(infoID byte, frames []downstream.Frame) {
	if !isIdentityID(infoID) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Deep copy frames for storage.
	stored := make([]downstream.Frame, len(frames))
	for i, f := range frames {
		payload := make([]byte, len(f.Payload))
		copy(payload, f.Payload)
		stored[i] = downstream.Frame{Command: f.Command, Payload: payload}
	}
	c.entries[infoID] = &infoCacheEntry{
		frames:    stored,
		updatedAt: time.Now(),
	}
}

// invalidateAll clears all cached identity entries (called on RESETTED).
func (c *adapterInfoCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[byte]*infoCacheEntry)
}
