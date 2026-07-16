package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.8.1", "0.8.1", 0},
		{"v0.8.1", "0.8.1", 0},
		{"0.8.0", "0.8.1", -1},
		{"0.8.1", "0.8.0", 1},
		{"0.9.0", "0.8.10", 1},
		{"0.8.10", "0.8.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.8.1-rc1", "0.8.1", 0},
		{"0.8", "0.8.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "hftools_1.2.3_linux_x86_64"},
		{"linux", "arm64", "hftools_1.2.3_linux_arm64"},
		{"darwin", "arm64", "hftools_1.2.3_macos_arm64"},
		{"windows", "amd64", "hftools_1.2.3_windows_x86_64.exe"},
	}
	for _, c := range cases {
		got, err := AssetName("1.2.3", c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("AssetName(1.2.3,%s,%s) = %q,%v want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	if _, err := AssetName("1.2.3", "plan9", "mips"); err == nil {
		t.Errorf("expected error for unsupported platform")
	}
}

func TestUpdateFlow(t *testing.T) {
	binary := []byte("BRAND-NEW-HFTOOLS-BINARY")
	sum := sha256.Sum256(binary)
	sumHex := hex.EncodeToString(sum[:])
	assetName, err := CurrentAssetName("1.2.3")
	if err != nil {
		t.Fatalf("asset name: %v", err)
	}
	sums := sumHex + "  " + assetName + "\n"

	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Release{
			TagName: "v1.2.3",
			Assets: []Asset{
				{Name: assetName, URL: base + "/dl/bin", Size: int64(len(binary))},
				{Name: "SHA256SUMS", URL: base + "/dl/sums", Size: int64(len(sums))},
			},
		})
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sums)) })
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL

	ctx := context.Background()
	client := server.Client()

	rel, err := LatestRelease(ctx, client, server.URL, "owner/repo", "")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if rel.Version() != "1.2.3" {
		t.Fatalf("version = %q", rel.Version())
	}
	if CompareVersions(rel.Version(), "1.2.0") <= 0 {
		t.Fatalf("expected 1.2.3 > 1.2.0")
	}

	checks, err := Checksums(ctx, client, rel)
	if err != nil {
		t.Fatalf("checksums: %v", err)
	}
	if checks[assetName] != sumHex {
		t.Fatalf("checksum lookup = %q", checks[assetName])
	}

	asset, ok := rel.FindAsset(assetName)
	if !ok {
		t.Fatalf("asset %q not found", assetName)
	}
	dir := t.TempDir()

	// Wrong checksum must be rejected and clean up.
	if _, err := DownloadVerified(ctx, client, asset, "deadbeef", dir); err == nil {
		t.Fatalf("expected checksum mismatch error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("temp file left behind after failed verify: %v", entries)
	}

	tmp, err := DownloadVerified(ctx, client, asset, checks[assetName], dir)
	if err != nil {
		t.Fatalf("download verified: %v", err)
	}

	// Replace a stand-in "current binary".
	dest := filepath.Join(dir, "hftools")
	if err := os.WriteFile(dest, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceExecutable(dest, tmp); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(binary) {
		t.Fatalf("dest content = %q (%v)", got, err)
	}
}

func TestLatestReleaseErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	if _, err := LatestRelease(context.Background(), server.Client(), server.URL, "owner/repo", ""); err == nil {
		t.Fatalf("expected error on 404")
	}
	_ = runtime.GOOS
}
