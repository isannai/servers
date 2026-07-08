package tunnel

import (
	"testing"
	"time"
)

func TestSessionExpiry(t *testing.T) {
	s, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(s.Token) != 16 {
		t.Errorf("token len = %d, want 16", len(s.Token))
	}
	if len(s.Key) != 32 {
		t.Errorf("key len = %d, want 32", len(s.Key))
	}
	if s.IsExpired(time.Now()) {
		t.Error("freshly issued session should not be expired")
	}
	// Just inside grace window → still OK.
	if s.IsExpired(s.ExpiresAt.Add(SessionGrace - time.Second)) {
		t.Error("session inside grace should not be expired")
	}
	// Past grace → expired.
	if !s.IsExpired(s.ExpiresAt.Add(SessionGrace + time.Second)) {
		t.Error("session past grace should be expired")
	}
}

func TestSessionCacheRotation(t *testing.T) {
	c := NewSessionCache()
	s1, _ := NewSession()
	s2, _ := NewSession()
	c.Put("P:0x1", s1)
	c.Put("P:0x1", s2)

	if e := c.LookupByToken(s2.Token); e == nil || e.Sess != s2 {
		t.Fatalf("new session should be looked up by token")
	}
	// Old token still valid within grace.
	if e := c.LookupByToken(s1.Token); e == nil {
		t.Fatalf("old session should be valid within grace")
	}
}

func TestRegisterDigestStable(t *testing.T) {
	d1 := RegisterDigest("P:0xabc", "cert", "v1", "bin", "owner", "hw", 1234567)
	d2 := RegisterDigest("P:0xabc", "cert", "v1", "bin", "owner", "hw", 1234567)
	if string(d1) != string(d2) {
		t.Fatal("digest should be deterministic for same inputs")
	}
	d3 := RegisterDigest("P:0xabc", "cert", "v1", "bin", "owner", "hw", 1234568)
	if string(d1) == string(d3) {
		t.Fatal("digest should change when timestamp changes")
	}
}
