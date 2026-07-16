package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTokenPrecedence(t *testing.T) {
	// Isolate from any real environment/token file.
	t.Setenv("HF_TOKEN", "")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HF_TOKEN_PATH", tokenFile)

	cfg := defaults()

	// Falls back to the huggingface_hub token file.
	if got := resolveToken(cfg); got != "file-token" {
		t.Fatalf("file fallback = %q, want file-token", got)
	}
	// Env var beats the file.
	t.Setenv("HF_TOKEN", "env-token")
	if got := resolveToken(cfg); got != "env-token" {
		t.Fatalf("env = %q, want env-token", got)
	}
	// Explicit --token beats everything.
	cfg.Token = "explicit"
	if got := resolveToken(cfg); got != "explicit" {
		t.Fatalf("explicit = %q, want explicit", got)
	}
}

func TestHFTokenFromFileMissing(t *testing.T) {
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "does-not-exist"))
	if got := hfTokenFromFile(); got != "" {
		t.Fatalf("expected empty token for missing file, got %q", got)
	}
}
