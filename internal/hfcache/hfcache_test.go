package hfcache

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ziozzang/hftools/internal/state"
)

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func gitSHA1(b []byte) string {
	h := sha1.New()
	_, _ = h.Write([]byte("blob " + strconv.Itoa(len(b)) + "\x00"))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func TestRepoFolderName(t *testing.T) {
	if got := RepoFolderName("model", "owner/name"); got != "models--owner--name" {
		t.Fatalf("model folder = %q", got)
	}
	if got := RepoFolderName("dataset", "owner/name"); got != "datasets--owner--name" {
		t.Fatalf("dataset folder = %q", got)
	}
	if got := RepoFolderName("", "bert-base-uncased"); got != "models--bert-base-uncased" {
		t.Fatalf("bare folder = %q", got)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildFlatRepo(t *testing.T) (dir string, m *state.Manifest, reg, lfs []byte) {
	t.Helper()
	dir = t.TempDir()
	reg = []byte("{\"hidden_size\": 4096}\n")
	lfs = make([]byte, 200<<10)
	for i := range lfs {
		lfs[i] = byte((i*37 + 5) % 251)
	}
	writeFile(t, filepath.Join(dir, "config.json"), reg)
	writeFile(t, filepath.Join(dir, "weights", "model.bin"), lfs)

	m = state.NewManifest("owner/model", "main", "0123456789abcdef0123456789abcdef01234567")
	m.RepoType = "model"
	m.Files["config.json"] = &state.FileRecord{
		Path: "config.json", Size: int64(len(reg)),
		RemoteBlobSHA1: gitSHA1(reg),
		LocalSHA256:    sha256hex(reg), LocalSHA1: "", LocalGitSHA1: gitSHA1(reg),
	}
	m.Files["weights/model.bin"] = &state.FileRecord{
		Path: "weights/model.bin", Size: int64(len(lfs)),
		RemoteLFSSHA256: sha256hex(lfs),
		LocalSHA256:     sha256hex(lfs), LocalGitSHA1: gitSHA1(lfs),
	}
	return dir, m, reg, lfs
}

func TestExportImportRoundTrip(t *testing.T) {
	src, m, reg, lfs := buildFlatRepo(t)
	cache := t.TempDir()

	exp, err := Export(ExportOptions{Manifest: m, SourceDir: src, CacheRoot: cache})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if exp.Files != 2 || exp.NewBlobs != 2 {
		t.Fatalf("export result = %+v, want 2 files / 2 blobs", exp)
	}

	storage := filepath.Join(cache, "models--owner--model")
	// Blobs are content-addressed by etag.
	if _, err := os.Stat(filepath.Join(storage, "blobs", gitSHA1(reg))); err != nil {
		t.Fatalf("regular blob missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storage, "blobs", sha256hex(lfs))); err != nil {
		t.Fatalf("lfs blob missing: %v", err)
	}
	// Snapshot entries are relative symlinks into ../blobs.
	pointer := filepath.Join(storage, "snapshots", m.CommitSHA, "weights", "model.bin")
	target, err := os.Readlink(pointer)
	if err != nil {
		t.Fatalf("snapshot pointer is not a symlink: %v", err)
	}
	if filepath.Base(target) != sha256hex(lfs) {
		t.Fatalf("pointer target = %q, want blob %s", target, sha256hex(lfs))
	}
	// refs/main records the commit.
	ref, err := os.ReadFile(filepath.Join(storage, "refs", "main"))
	if err != nil || string(ref) != m.CommitSHA {
		t.Fatalf("refs/main = %q, %v; want %s", ref, err, m.CommitSHA)
	}

	// Import back into a fresh flat directory.
	dst := t.TempDir()
	m2, imp, err := Import(ImportOptions{CacheRoot: cache, RepoID: "owner/model", RepoType: "model", Revision: "main", DestDir: dst})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imp.Commit != m.CommitSHA || imp.Files != 2 {
		t.Fatalf("import result = %+v", imp)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "config.json")); string(got) != string(reg) {
		t.Fatal("imported config.json differs")
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "weights", "model.bin")); string(got) != string(lfs) {
		t.Fatal("imported model.bin differs")
	}
	// Reconstructed manifest classifies LFS vs regular correctly.
	if rec := m2.Files["weights/model.bin"]; rec == nil || rec.RemoteLFSSHA256 != sha256hex(lfs) {
		t.Fatalf("lfs record wrong: %+v", rec)
	}
	if rec := m2.Files["config.json"]; rec == nil || rec.RemoteLFSSHA256 != "" || rec.RemoteBlobSHA1 != gitSHA1(reg) {
		t.Fatalf("regular record wrong: %+v", rec)
	}
}

func TestImportDetectsCorruptedBlob(t *testing.T) {
	src, m, _, _ := buildFlatRepo(t)
	cache := t.TempDir()
	if _, err := Export(ExportOptions{Manifest: m, SourceDir: src, CacheRoot: cache}); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Tamper with a blob so its content no longer matches its etag name.
	blob := filepath.Join(cache, "models--owner--model", "blobs", gitSHA1([]byte("{\"hidden_size\": 4096}\n")))
	if err := os.WriteFile(blob, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, _, err := Import(ImportOptions{CacheRoot: cache, RepoID: "owner/model", RepoType: "model", Revision: "main", DestDir: dst}); err == nil {
		t.Fatal("expected import to reject a blob that does not match its name")
	}
}

func TestExportCopyMode(t *testing.T) {
	src, m, reg, _ := buildFlatRepo(t)
	cache := t.TempDir()
	if _, err := Export(ExportOptions{Manifest: m, SourceDir: src, CacheRoot: cache, Copy: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	// In copy mode the blob is an independent file (not a hardlink of the source).
	blob := filepath.Join(cache, "models--owner--model", "blobs", gitSHA1(reg))
	si, err := os.Stat(filepath.Join(src, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(si, bi) && bi.Size() != si.Size() {
		t.Fatalf("copied blob size mismatch")
	}
}
