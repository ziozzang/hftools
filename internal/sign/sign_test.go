package sign

import (
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	msg := []byte("# hftools SHA-256\nabc  model.safetensors\n")
	rec := Sign(msg, priv, "alice", time.Unix(1700000000, 0).UTC())
	if err := rec.Verify(msg, pub); err != nil {
		t.Fatalf("verify pinned: %v", err)
	}
	if err := rec.Verify(msg, nil); err != nil {
		t.Fatalf("verify unpinned: %v", err)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	pub, priv, _ := GenerateKey()
	rec := Sign([]byte("original"), priv, "", time.Unix(1, 0))
	if err := rec.Verify([]byte("tampered"), pub); err == nil {
		t.Fatalf("expected verification failure on modified payload")
	}
}

func TestVerifyRejectsWrongPinnedKey(t *testing.T) {
	_, priv, _ := GenerateKey()
	other, _, _ := GenerateKey()
	msg := []byte("payload")
	rec := Sign(msg, priv, "", time.Unix(1, 0))
	if err := rec.Verify(msg, other); err == nil {
		t.Fatalf("expected rejection when pinned key differs")
	}
}

func TestPEMRoundTrip(t *testing.T) {
	_, priv, _ := GenerateKey()
	pemBytes, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParsePrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Equal(priv) {
		t.Fatalf("private key round-trip mismatch")
	}
}

func TestParsePublicKeyHex(t *testing.T) {
	pub, _, _ := GenerateKey()
	parsed, err := ParsePublicKey(PublicKeyHex(pub))
	if err != nil {
		t.Fatalf("parse hex: %v", err)
	}
	if !parsed.Equal(pub) {
		t.Fatalf("public key hex round-trip mismatch")
	}
}
