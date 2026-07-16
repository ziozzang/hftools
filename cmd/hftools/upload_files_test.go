package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestCollectUploadFilesSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No prefix: destination is the basename.
	files, err := collectUploadFiles([]string{p}, "")
	if err != nil || len(files) != 1 || files[0].PathInRepo != "model.bin" {
		t.Fatalf("single file = %+v, %v", files, err)
	}
	// Prefix acts as the full destination for a single file.
	files, err = collectUploadFiles([]string{p}, "sub/renamed.bin")
	if err != nil || files[0].PathInRepo != "sub/renamed.bin" {
		t.Fatalf("single file with prefix = %+v, %v", files, err)
	}
}

func TestCollectUploadFilesDirectory(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "a")
	mustWrite(t, filepath.Join(dir, "nested", "b.txt"), "b")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "ignored")

	files, err := collectUploadFiles([]string{dir}, "root")
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	var dests []string
	for _, f := range files {
		dests = append(dests, f.PathInRepo)
	}
	sort.Strings(dests)
	if len(dests) != 2 || dests[0] != "root/a.txt" || dests[1] != "root/nested/b.txt" {
		t.Fatalf("dir dests = %v (should skip .git)", dests)
	}
}

func TestCollectUploadFilesMultiple(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	mustWrite(t, a, "a")
	mustWrite(t, b, "b")
	files, err := collectUploadFiles([]string{a, b}, "data")
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	// A directory among multiple explicit paths is rejected.
	if _, err := collectUploadFiles([]string{a, dir}, ""); err == nil {
		t.Fatalf("expected error for directory in multi-file mode")
	}
}

func TestValidateRepoPath(t *testing.T) {
	for _, bad := range []string{"", "/abs", "..", "../x", "a/../../b", "a/.."} {
		if err := validateRepoPath(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
	for _, ok := range []string{"a", "a/b", "a/b.c", "a..b/c"} {
		if err := validateRepoPath(ok); err != nil {
			t.Errorf("expected %q to be accepted: %v", ok, err)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
