package uploader

import (
	"os"
	"strings"
	"sync"
)

// keyRing holds an ordered list of API keys and rotates through them when a
// key is invalidated (auth failure, storage quota exhausted, rate limit).  It
// mirrors the imgbbKeyRing pattern but is generic so video-host uploaders can
// each keep their own ring.
//
// Keys are provided as a comma-separated string.  A single key yields a
// ring of length 1 whose rotate() is a no-op, so callers do not need to
// special-case the single-key case.
type keyRing struct {
	mu    sync.Mutex
	keys  []string
	index int
}

// newKeyRing builds a ring from a comma-separated key list.
func newKeyRing(commaSeparated string) *keyRing {
	var keys []string
	for _, k := range strings.Split(commaSeparated, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}
	return &keyRing{keys: keys}
}

// current returns the active key, or "" when no keys are configured.
func (kr *keyRing) current() string {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.keys) == 0 {
		return ""
	}
	return kr.keys[kr.index]
}

// rotate advances to the next key in the ring.  No-op for a single-key ring.
func (kr *keyRing) rotate() {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.keys) > 1 {
		kr.index = (kr.index + 1) % len(kr.keys)
	}
}

// count returns the number of keys in the ring.
func (kr *keyRing) count() int {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return len(kr.keys)
}

// buildRingFromEnv constructs a keyRing from an env var, falling back to the
// passed single key when the env var is empty or holds only one value.  This
// keeps existing single-key deployments working while allowing operators to
// add rotation just by listing multiple keys in the env var.
func buildRingFromEnv(envVar, singleKey string) *keyRing {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return newKeyRing(singleKey)
	}
	ring := newKeyRing(raw)
	if ring.count() == 0 {
		return newKeyRing(singleKey)
	}
	return ring
}
