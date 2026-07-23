package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ziozzang/hftools/internal/sign"
)

func TestConfigYAMLRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HFTOOLS_HOME", home)

	pub1, _, _ := sign.GenerateKey()
	pub2, _, _ := sign.GenerateKey()
	cfg := &Config{Signer: "jung@jioh.net", KeyPath: "~/.hftools/signing.key"}
	cfg.Trust("alice", pub1)
	cfg.Trust("bob", pub2)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, path, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if path != filepath.Join(home, ConfigName) {
		t.Fatalf("unexpected config path %s", path)
	}
	if got.Signer != "jung@jioh.net" {
		t.Fatalf("signer = %q", got.Signer)
	}
	if got.KeyPath != "~/.hftools/signing.key" {
		t.Fatalf("key_path = %q", got.KeyPath)
	}
	if name := got.LookupTrustedName(pub1); name != "alice" {
		t.Fatalf("pub1 trusted name = %q, want alice", name)
	}
	if name := got.LookupTrustedName(pub2); name != "bob" {
		t.Fatalf("pub2 trusted name = %q, want bob", name)
	}
}

func TestLoadConfigMissingReturnsDefaults(t *testing.T) {
	t.Setenv("HFTOOLS_HOME", t.TempDir())
	cfg, _, err := LoadConfig()
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if cfg.Signer != "" || len(cfg.TrustedKeys) != 0 {
		t.Fatalf("expected empty defaults, got %+v", cfg)
	}
}

func TestEnsureKeyGeneratesThenReuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HFTOOLS_HOME", home)
	cfg := &Config{}

	priv1, path, created, err := EnsureKey(cfg)
	if err != nil || !created {
		t.Fatalf("first EnsureKey: created=%v err=%v", created, err)
	}
	if path != filepath.Join(home, KeyName) {
		t.Fatalf("key path = %s", path)
	}
	if _, err := os.Stat(filepath.Join(home, PubName)); err != nil {
		t.Fatalf("public key not written: %v", err)
	}

	priv2, _, created2, err := EnsureKey(cfg)
	if err != nil || created2 {
		t.Fatalf("second EnsureKey: created=%v err=%v", created2, err)
	}
	if !priv1.Equal(priv2) {
		t.Fatalf("key regenerated instead of reused")
	}
}

func TestResolvePinnedByNameFileHex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HFTOOLS_HOME", home)
	pub, _, _ := sign.GenerateKey()
	cfg := &Config{}
	cfg.Trust("alice", pub)

	// by trusted name
	got, label, err := cfg.ResolvePinned("alice")
	if err != nil || label != "alice" || !got.Equal(pub) {
		t.Fatalf("by name: label=%q err=%v", label, err)
	}
	// by hex, recognized as a trusted key
	got, label, err = cfg.ResolvePinned(sign.PublicKeyHex(pub))
	if err != nil || label != "alice" || !got.Equal(pub) {
		t.Fatalf("by hex: label=%q err=%v", label, err)
	}
	// by PEM file
	pemBytes, _ := sign.MarshalPublicKeyPEM(pub)
	pemPath := filepath.Join(home, "k.pem")
	if err := os.WriteFile(pemPath, pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err = cfg.ResolvePinned(pemPath)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("by file: err=%v", err)
	}
	// empty means no pin
	if got, _, err := cfg.ResolvePinned(""); err != nil || got != nil {
		t.Fatalf("empty spec should yield nil pin, got %v err %v", got, err)
	}
}

func TestParseYAMLIgnoresUnknownAndComments(t *testing.T) {
	data := []byte("# header\nsigner: me@example.com # inline\nfuture_field: whatever\ntrusted_keys:\n  # a comment\n  alice: abcdef\n")
	cfg, err := parseYAML(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Signer != "me@example.com" {
		t.Fatalf("signer = %q", cfg.Signer)
	}
	if cfg.TrustedKeys["alice"] != "abcdef" {
		t.Fatalf("alice = %q", cfg.TrustedKeys["alice"])
	}
}
