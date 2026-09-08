package uploader

import "testing"

func TestKeyRingSingleKeyNoRotate(t *testing.T) {
	kr := newKeyRing("single-key")
	if kr.count() != 1 {
		t.Fatalf("count = %d, want 1", kr.count())
	}
	if kr.current() != "single-key" {
		t.Fatalf("current = %q, want %q", kr.current(), "single-key")
	}
	kr.rotate()
	if kr.current() != "single-key" {
		t.Fatalf("rotate on single-key ring changed key to %q, want %q", kr.current(), "single-key")
	}
}

func TestKeyRingMultiKeyRotates(t *testing.T) {
	kr := newKeyRing("k1,k2,k3")
	if kr.count() != 3 {
		t.Fatalf("count = %d, want 3", kr.count())
	}
	if kr.current() != "k1" {
		t.Fatalf("current = %q, want k1", kr.current())
	}
	kr.rotate()
	if kr.current() != "k2" {
		t.Fatalf("after one rotate current = %q, want k2", kr.current())
	}
	kr.rotate()
	kr.rotate()
	if kr.current() != "k1" {
		t.Fatalf("after wrapping current = %q, want k1", kr.current())
	}
}

func TestKeyRingIgnoresEmptyAndWhitespace(t *testing.T) {
	kr := newKeyRing("k1, ,")
	if kr.count() != 1 {
		t.Fatalf("count = %d, want 1", kr.count())
	}
	if kr.current() != "k1" {
		t.Fatalf("current = %q, want k1", kr.current())
	}
}

func TestKeyRingEmpty(t *testing.T) {
	kr := newKeyRing("")
	if kr.count() != 0 {
		t.Fatalf("count = %d, want 0", kr.count())
	}
	if kr.current() != "" {
		t.Fatalf("current = %q, want empty", kr.current())
	}
}

func TestBuildRingFromEnvFallsBackToSingleKey(t *testing.T) {
	t.Setenv("TEST_KEY_RING_ENV", "")
	kr := buildRingFromEnv("TEST_KEY_RING_ENV", "fallback-key")
	if kr.count() != 1 || kr.current() != "fallback-key" {
		t.Fatalf("env empty: got count=%d current=%q, want 1/fallback-key", kr.count(), kr.current())
	}

	t.Setenv("TEST_KEY_RING_ENV", "env1,env2")
	kr = buildRingFromEnv("TEST_KEY_RING_ENV", "fallback-key")
	if kr.count() != 2 || kr.current() != "env1" {
		t.Fatalf("env keys: got count=%d current=%q, want 2/env1", kr.count(), kr.current())
	}

	t.Setenv("TEST_KEY_RING_ENV", "   ")
	kr = buildRingFromEnv("TEST_KEY_RING_ENV", "fallback-key")
	if kr.count() != 1 || kr.current() != "fallback-key" {
		t.Fatalf("env whitespace: got count=%d current=%q, want 1/fallback-key", kr.count(), kr.current())
	}
}

func TestVoeSXUploaderRotatesOnAuthError(t *testing.T) {
	t.Setenv("VOESX_API_KEY", "bad-key,good-key")
	u := NewVoeSXUploader("bad-key,good-key")
	if u.keys.count() != 2 {
		t.Fatalf("key count = %d, want 2", u.keys.count())
	}
	if u.keys.current() != "bad-key" {
		t.Fatalf("current = %q, want bad-key", u.keys.current())
	}
	// Simulate the rotation that happens on an auth error (as in UploadWithProgress).
	u.keys.rotate()
	if u.keys.current() != "good-key" {
		t.Fatalf("after rotate current = %q, want good-key", u.keys.current())
	}
}

func TestVidaraUploaderRotatesOnAuthError(t *testing.T) {
	t.Setenv("VIDARA_KEY", "bad-key,good-key")
	u := NewVidaraUploader("bad-key,good-key")
	if u.keys.count() != 2 {
		t.Fatalf("key count = %d, want 2", u.keys.count())
	}
	if u.keys.current() != "bad-key" {
		t.Fatalf("current = %q, want bad-key", u.keys.current())
	}
	u.keys.rotate()
	if u.keys.current() != "good-key" {
		t.Fatalf("after rotate current = %q, want good-key", u.keys.current())
	}
}
