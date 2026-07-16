package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/hftools/internal/modelfile"
)

// buildSafetensors returns a minimal but valid safetensors file: an 8-byte
// little-endian header length, the JSON header, then the tensor data.
func makeSafetensors() []byte {
	header := `{"__metadata__":{"format":"pt"},"w":{"dtype":"F32","shape":[2,8],"data_offsets":[0,64]}}`
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(len(header)))
	out = append(out, header...)
	out = append(out, make([]byte, 64)...) // tensor bytes
	return out
}

// fakeHub serves a two-file repository, answering both full GETs (get/Fetch)
// and Range requests (download/peek).
func fakeHub(t *testing.T, files map[string][]byte, blobs map[string]map[string]any, commit string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/revision/main") {
			var siblings []map[string]any
			for name := range files {
				sb := map[string]any{"rfilename": name, "size": len(files[name])}
				for k, v := range blobs[name] {
					sb[k] = v
				}
				siblings = append(siblings, sb)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "owner/repo", "sha": commit, "siblings": siblings})
			return
		}
		prefix := "/owner/repo/resolve/" + commit + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		content, ok := files[r.URL.Path[len(prefix):]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err == nil {
			if start < 0 || end >= len(content) {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start : end+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write(content)
	}))
}

func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), err
}

func TestRemoteToolsRoundTrip(t *testing.T) {
	cfg := []byte("{\"hidden_size\": 8}\n")
	st := makeSafetensors()
	const commit = "abc0000000000000000000000000000000000abc"
	files := map[string][]byte{"config.json": cfg, "model.safetensors": st}
	blobs := map[string]map[string]any{
		"config.json":       {"blobId": gitBlobID(cfg)},
		"model.safetensors": {"lfs": map[string]any{"sha256": sha256Hex(st), "size": len(st)}},
	}
	server := fakeHub(t, files, blobs, commit)
	defer server.Close()
	ctx := context.Background()

	// info --json
	out, err := capture(t, func() error {
		return run(ctx, []string{"info", "--endpoint", server.URL, "--json", "owner/repo"})
	})
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	var rep infoReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("info json: %v (%s)", err, out)
	}
	if rep.Files != 2 || rep.LFSFiles != 1 {
		t.Fatalf("info report = %+v", rep)
	}

	// ls plain
	if err := run(ctx, []string{"ls", "--endpoint", server.URL, "owner/repo"}); err != nil {
		t.Fatalf("ls: %v", err)
	}

	// get config.json to a file
	base := t.TempDir()
	dst := filepath.Join(base, "config.json")
	if err := run(ctx, []string{"get", "--endpoint", server.URL, "-o", dst, "owner/repo", "config.json"}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(cfg) {
		t.Fatalf("get content mismatch: %q", got)
	}

	// peek safetensors --json
	out, err = capture(t, func() error {
		return run(ctx, []string{"peek", "--endpoint", server.URL, "--json", "owner/repo", "model.safetensors"})
	})
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	var info modelfile.Info
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("peek json: %v (%s)", err, out)
	}
	if info.Format != modelfile.FormatSafetensors || info.Tensors != 1 || info.Params != 16 {
		t.Fatalf("peek info = %+v", info)
	}

	// download, then diff (unchanged) and du
	flat := filepath.Join(base, "owner_repo")
	if err := run(ctx, []string{"download", "--endpoint", server.URL, "--output", flat,
		"--parts", "2", "--multipart-threshold", "1B", "owner/repo"}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if err := run(ctx, []string{"diff", "--endpoint", server.URL, "--output", flat}); err != nil {
		t.Fatalf("diff: %v", err)
	}
	out, err = capture(t, func() error { return run(ctx, []string{"du", "--root", base, "--json"}) })
	if err != nil {
		t.Fatalf("du: %v", err)
	}
	var du duReport
	if err := json.Unmarshal([]byte(out), &du); err != nil || du.Total == 0 {
		t.Fatalf("du report = %s (%v)", out, err)
	}

	// sign + verify-sig round trip
	key := filepath.Join(base, "key.pem")
	if err := run(ctx, []string{"sign", "--output", flat, "--gen-key", key}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := run(ctx, []string{"verify-sig", "--output", flat}); err != nil {
		t.Fatalf("verify-sig: %v", err)
	}
	// Tampering with the signed .sha256 must fail verification.
	shaPath := filepath.Join(flat, ".sha256")
	orig, _ := os.ReadFile(shaPath)
	_ = os.WriteFile(shaPath, append([]byte("# tampered\n"), orig...), 0o644)
	if err := run(ctx, []string{"verify-sig", "--output", flat}); err == nil {
		t.Fatalf("verify-sig should fail on tampered manifest")
	}
}

func TestScanCommandFlagsMaliciousPickle(t *testing.T) {
	dir := t.TempDir()
	// A pickle that imports os.system via STACK_GLOBAL.
	var p []byte
	p = append(p, 0x80, 0x04) // PROTO 4
	push := func(s string) {
		p = append(p, 0x8c, byte(len(s)))
		p = append(p, s...)
		p = append(p, 0x94)
	}
	push("os")
	push("system")
	p = append(p, 0x93, '.') // STACK_GLOBAL, STOP
	if err := os.WriteFile(filepath.Join(dir, "evil.pkl"), p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"scan", dir}); err == nil {
		t.Fatalf("scan should report a critical finding and return an error")
	}
}
