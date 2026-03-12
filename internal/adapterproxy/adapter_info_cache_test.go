package adapterproxy

import (
	"testing"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestIsIdentityID(t *testing.T) {
	tests := []struct {
		id   byte
		want bool
	}{
		{0x00, true},  // Version
		{0x01, true},  // HW ID
		{0x02, true},  // HW Config
		{0x03, false}, // Temperature (telemetry)
		{0x04, false}, // Supply voltage
		{0x05, false}, // Bus voltage
		{0x06, false}, // Reset info
		{0x07, false}, // WiFi RSSI
	}
	for _, tt := range tests {
		if got := isIdentityID(tt.id); got != tt.want {
			t.Errorf("isIdentityID(0x%02x) = %v; want %v", tt.id, got, tt.want)
		}
	}
}

func TestCacheGetMiss(t *testing.T) {
	c := newAdapterInfoCache()
	if frames := c.get(0x00); frames != nil {
		t.Errorf("expected nil for cold cache, got %v", frames)
	}
}

func TestCachePutAndGet(t *testing.T) {
	c := newAdapterInfoCache()
	infoCmd := byte(0x0A) // ENHResInfo
	frames := []downstream.Frame{
		{Command: infoCmd, Payload: []byte{0x02}}, // length=2
		{Command: infoCmd, Payload: []byte{0x23}}, // version
		{Command: infoCmd, Payload: []byte{0x01}}, // features
	}
	c.put(0x00, frames)

	got := c.get(0x00)
	if got == nil {
		t.Fatal("expected cached frames, got nil")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(got))
	}
	if got[1].Payload[0] != 0x23 {
		t.Errorf("version byte = 0x%02x; want 0x23", got[1].Payload[0])
	}
}

func TestCacheDeepCopy(t *testing.T) {
	c := newAdapterInfoCache()
	original := []downstream.Frame{
		{Command: 0x0A, Payload: []byte{0x01}},
		{Command: 0x0A, Payload: []byte{0xFF}},
	}
	c.put(0x00, original)

	// Mutate original — should not affect cache.
	original[1].Payload[0] = 0x00

	got := c.get(0x00)
	if got[1].Payload[0] != 0xFF {
		t.Errorf("cache was mutated via original slice; got 0x%02x want 0xFF", got[1].Payload[0])
	}

	// Mutate returned copy — should not affect cache.
	got[1].Payload[0] = 0xAA
	got2 := c.get(0x00)
	if got2[1].Payload[0] != 0xFF {
		t.Errorf("cache was mutated via returned slice; got 0x%02x want 0xFF", got2[1].Payload[0])
	}
}

func TestCacheTelemetryIDsNotCached(t *testing.T) {
	c := newAdapterInfoCache()
	frames := []downstream.Frame{
		{Command: 0x0A, Payload: []byte{0x02}},
		{Command: 0x0A, Payload: []byte{0x00}},
		{Command: 0x0A, Payload: []byte{0x2D}},
	}
	c.put(0x03, frames) // Temperature — telemetry, should not cache

	if got := c.get(0x03); got != nil {
		t.Errorf("telemetry ID 0x03 should not be cached, got %v", got)
	}
}

func TestCacheInvalidateAll(t *testing.T) {
	c := newAdapterInfoCache()
	for id := byte(0x00); id <= 0x02; id++ {
		c.put(id, []downstream.Frame{
			{Command: 0x0A, Payload: []byte{0x01}},
			{Command: 0x0A, Payload: []byte{id}},
		})
	}

	// Verify all cached.
	for id := byte(0x00); id <= 0x02; id++ {
		if c.get(id) == nil {
			t.Fatalf("ID 0x%02x should be cached before invalidation", id)
		}
	}

	c.invalidateAll()

	// Verify all cleared.
	for id := byte(0x00); id <= 0x02; id++ {
		if c.get(id) != nil {
			t.Errorf("ID 0x%02x should be nil after invalidation", id)
		}
	}
}

func TestCacheOverwrite(t *testing.T) {
	c := newAdapterInfoCache()
	c.put(0x00, []downstream.Frame{
		{Command: 0x0A, Payload: []byte{0x01}},
		{Command: 0x0A, Payload: []byte{0xAA}},
	})
	c.put(0x00, []downstream.Frame{
		{Command: 0x0A, Payload: []byte{0x01}},
		{Command: 0x0A, Payload: []byte{0xBB}},
	})

	got := c.get(0x00)
	if got[1].Payload[0] != 0xBB {
		t.Errorf("overwrite failed; got 0x%02x want 0xBB", got[1].Payload[0])
	}
}
