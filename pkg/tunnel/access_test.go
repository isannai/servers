package tunnel

import "testing"

func TestAccessMessageRoundTrip(t *testing.T) {
	// bound: buyer set.
	msg := AccessMessage("0xAbCd00000000000000000000000000000000EfFf", 1700000000000, 1800000000000)
	if !IsAccessMessage(msg) {
		t.Fatalf("IsAccessMessage false for %q", msg)
	}
	// distinct from the RV admission prefix (no cross-door replay).
	if IsAccessMessage(CredentialMessage("0xabcd", 1, 2)) {
		t.Error("an ISANN-CREDENTIAL message must NOT parse as an access message")
	}
	buyer, issued, expire, ok := ParseAccessMessage(msg)
	if !ok {
		t.Fatal("ParseAccessMessage ok=false")
	}
	if buyer != "0xabcd00000000000000000000000000000000efff" { // lowercased
		t.Errorf("buyer = %q, want lowercased", buyer)
	}
	if issued != 1700000000000 || expire != 1800000000000 {
		t.Errorf("issued/expire = %d/%d", issued, expire)
	}

	// bearer: empty buyer parses ok with buyer="".
	b2, _, _, ok2 := ParseAccessMessage(AccessMessage("", 1, 2))
	if !ok2 || b2 != "" {
		t.Errorf("bearer parse: ok=%v buyer=%q, want true/empty", ok2, b2)
	}
}

func TestParseAccessMessageRejects(t *testing.T) {
	for _, bad := range []string{
		"ISANN-CREDENTIAL:0xabcd:1:2", // wrong prefix
		"ISANN-ACCESS:0xabcd:1",       // too few parts
		"ISANN-ACCESS:0xabcd:x:2",     // non-numeric issued
		"ISANN-ACCESS:0xb:1:2:3",      // too many parts
		"garbage",
	} {
		if _, _, _, ok := ParseAccessMessage(bad); ok {
			t.Errorf("ParseAccessMessage(%q) ok=true, want false", bad)
		}
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	msg := AccessMessage("", 111, 222)
	sig := "3bcb10b696ca16de" // hex-ish stub
	tok := EncodeAccessToken(msg, sig)
	if tok[:len(AccessTokenPrefix)] != AccessTokenPrefix {
		t.Errorf("token missing prefix: %q", tok)
	}
	gotMsg, gotSig, err := DecodeAccessToken(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotMsg != msg || gotSig != sig {
		t.Errorf("round-trip mismatch: msg=%q sig=%q", gotMsg, gotSig)
	}
}

func TestCredentialMessageRoundTrip(t *testing.T) {
	// node-bound
	msg := CredentialMessage("0xAbCd", 111, 222)
	node, issued, expire, ok := ParseCredentialMessage(msg)
	if !ok || node != "0xabcd" || issued != 111 || expire != 222 {
		t.Errorf("parse = %q/%d/%d ok=%v", node, issued, expire, ok)
	}
	// bearer (empty node)
	n2, _, _, ok2 := ParseCredentialMessage(CredentialMessage("", 1, 2))
	if !ok2 || n2 != "" {
		t.Errorf("bearer parse: node=%q ok=%v", n2, ok2)
	}
	// an ISANN-ACCESS message must NOT parse as a credential (cred add rejects it).
	if _, _, _, ok := ParseCredentialMessage(AccessMessage("0xb", 1, 2)); ok {
		t.Error("ISANN-ACCESS must not parse as ISANN-CREDENTIAL")
	}
	// token round-trip: same ianacc_ codec carries the cred message + sig.
	tok := EncodeAccessToken(msg, "deadbeef")
	m, s, err := DecodeAccessToken(tok)
	if err != nil || m != msg || s != "deadbeef" {
		t.Errorf("token round-trip: m=%q s=%q err=%v", m, s, err)
	}
}

func TestDecodeAccessTokenRejects(t *testing.T) {
	for _, bad := range []string{
		"3bcb10b6",         // raw sig, no prefix
		"ianacc_!!!notb64", // bad base64
		"ianacc_" + "e30",  // base64 of "{}" → empty msg/sig
	} {
		if _, _, err := DecodeAccessToken(bad); err == nil {
			t.Errorf("DecodeAccessToken(%q) err=nil, want error", bad)
		}
	}
}
