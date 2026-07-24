package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/hftools/internal/identity"
)

// maliciousPickle returns a pickle that imports os.system via STACK_GLOBAL.
func maliciousPickle() []byte {
	p := []byte{0x80, 0x04}
	push := func(s string) {
		p = append(p, 0x8c, byte(len(s)))
		p = append(p, s...)
		p = append(p, 0x94)
	}
	push("os")
	push("system")
	return append(p, 0x93, '.')
}

func TestExtraRepoChecksScanFlagsMalicious(t *testing.T) {
	t.Setenv("HFTOOLS_HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "evil.pkl"), maliciousPickle(), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	errs := extraRepoChecks(context.Background(), repo, true, false, cfg, "", false)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "dangerous") {
		t.Fatalf("expected a dangerous-import failure, got %v", errs)
	}

	// A clean directory passes.
	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "readme.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if errs := extraRepoChecks(context.Background(), clean, true, false, cfg, "", false); len(errs) != 0 {
		t.Fatalf("clean dir should pass scan, got %v", errs)
	}
}

func TestExtraRepoChecksVerifiesSignature(t *testing.T) {
	t.Setenv("HFTOOLS_HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".sha256"), []byte("# hftools SHA-256\nabc  model.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(repo, ".metadata")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := autoSignRepo(repo, stateDir); err != nil {
		t.Fatalf("autoSignRepo: %v", err)
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if errs := extraRepoChecks(context.Background(), repo, false, true, cfg, "", false); len(errs) != 0 {
		t.Fatalf("signature should verify, got %v", errs)
	}
}

func TestScanInterruptedByCanceledContext(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.pkl"), maliciousPickle(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already interrupted
	if _, _, _, err := scanRepositoryDir(ctx, repo); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
