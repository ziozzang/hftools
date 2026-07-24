package main

import (
	"strings"
	"testing"

	"github.com/ziozzang/hftools/internal/hub"
	"github.com/ziozzang/hftools/internal/identity"
	"github.com/ziozzang/hftools/internal/sign"
	"github.com/ziozzang/hftools/internal/state"
)

// remoteMatchesRecord must answer "did upstream change this file?" independently
// of local disk state, so a merely touched file is not reported as changed.
func TestRemoteMatchesRecord(t *testing.T) {
	remote := hub.RepoFile{Path: "model.bin", Size: 100, BlobID: "blob1"}
	rec := &state.FileRecord{Path: "model.bin", Size: 100, RemoteBlobSHA1: "blob1"}

	if !remoteMatchesRecord(rec, remote) {
		t.Fatalf("unchanged remote object should match its record")
	}
	if remoteMatchesRecord(nil, remote) {
		t.Fatalf("a missing record is never a match")
	}

	changedBlob := *rec
	changedBlob.RemoteBlobSHA1 = "blob2"
	if remoteMatchesRecord(&changedBlob, remote) {
		t.Fatalf("a different blob id must count as changed upstream")
	}

	changedSize := *rec
	changedSize.Size = 101
	if remoteMatchesRecord(&changedSize, remote) {
		t.Fatalf("a different size must count as changed upstream")
	}

	// The record's local mtime is irrelevant here: only the remote object is
	// being compared, which is what separates this from recordCurrent.
	touched := *rec
	touched.ModTimeUnixNano = 12345
	if !remoteMatchesRecord(&touched, remote) {
		t.Fatalf("local mtime must not affect the upstream comparison")
	}
}

func TestRemoteMatchesRecordLFS(t *testing.T) {
	remote := hub.RepoFile{Path: "w.safetensors", Size: 10, BlobID: "b", LFS: &hub.LFSInfo{SHA256: "aaa", Size: 10}}
	rec := &state.FileRecord{Path: "w.safetensors", Size: 10, RemoteBlobSHA1: "b", RemoteLFSSHA256: "aaa"}
	if !remoteMatchesRecord(rec, remote) {
		t.Fatalf("matching LFS object should match")
	}
	rotated := *rec
	rotated.RemoteLFSSHA256 = "bbb"
	if remoteMatchesRecord(&rotated, remote) {
		t.Fatalf("a different LFS sha256 must count as changed upstream")
	}
	// A record made before the file became LFS-tracked must not match.
	plain := &state.FileRecord{Path: "w.safetensors", Size: 10, RemoteBlobSHA1: "b"}
	if remoteMatchesRecord(plain, remote) {
		t.Fatalf("a record without the LFS hash must count as changed")
	}
}

// A pre-v2 record's signer label is forgeable — anyone can rewrite a v2 record
// as v1 and change it — so enforcement must reject it while accepting a record
// whose identity the signature actually covers.
func TestCheckSignedIdentity(t *testing.T) {
	legacy := sign.Record{Version: 1, Signer: "mallory@evil.example"}
	modern := sign.Record{Version: 2, Signer: "alice@corp.example", MetadataSignature: "aa"}

	// Off by default: the caller only gets the warning in the display.
	if err := checkSignedIdentity(legacy, &identity.Config{}, false); err != nil {
		t.Fatalf("unenforced legacy record should not error: %v", err)
	}
	// Enforced by flag.
	if err := checkSignedIdentity(legacy, &identity.Config{}, true); err == nil {
		t.Fatalf("expected the flag to reject an unauthenticated signer label")
	}
	// Enforced by config.
	if err := checkSignedIdentity(legacy, &identity.Config{RequireSignedIdentity: true}, false); err == nil {
		t.Fatalf("expected config.yaml to reject an unauthenticated signer label")
	}
	// A signature-covered identity passes under every setting.
	for _, flagSet := range []bool{false, true} {
		if err := checkSignedIdentity(modern, &identity.Config{RequireSignedIdentity: true}, flagSet); err != nil {
			t.Fatalf("a v2 record must pass enforcement: %v", err)
		}
	}
	// A nil config must not panic.
	if err := checkSignedIdentity(legacy, nil, false); err != nil {
		t.Fatalf("nil config should be permissive, got %v", err)
	}
}

// The unverified label must be visibly marked, so it cannot be skimmed as proof.
func TestSignerLineMarksUnverified(t *testing.T) {
	legacy := signerLine(sign.Record{Version: 1, Signer: "mallory@evil.example"})
	if !strings.Contains(legacy, "UNVERIFIED") {
		t.Fatalf("pre-v2 label must be marked UNVERIFIED, got %q", legacy)
	}
	modern := signerLine(sign.Record{Version: 2, Signer: "alice@corp.example", MetadataSignature: "aa"})
	if strings.Contains(modern, "UNVERIFIED") || !strings.Contains(modern, "alice@corp.example") {
		t.Fatalf("a signature-covered label must render plainly, got %q", modern)
	}
}
