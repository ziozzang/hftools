package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateNoticeText(t *testing.T) {
	if got := updateNoticeText("0.9.1", "0.9.0"); got == "" {
		t.Fatalf("expected a notice when newer version available")
	}
	if got := updateNoticeText("0.9.0", "0.9.0"); got != "" {
		t.Fatalf("expected no notice when up to date, got %q", got)
	}
	if got := updateNoticeText("0.8.0", "0.9.0"); got != "" {
		t.Fatalf("expected no notice when local is newer, got %q", got)
	}
}

func TestNotifyExemptCommands(t *testing.T) {
	for _, c := range []string{"", "update", "version", "help", "completion", "-h"} {
		if !notifyExemptCommands[c] {
			t.Errorf("command %q should be exempt from update notice", c)
		}
	}
	for _, c := range []string{"download", "info", "peek", "get"} {
		if notifyExemptCommands[c] {
			t.Errorf("command %q should NOT be exempt", c)
		}
	}
}

func TestUpdateCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "update-check.json")
	want := updateCache{CheckedAt: time.Unix(1700000000, 0).UTC(), Latest: "1.2.3"}
	writeUpdateCache(path, want)
	got := readUpdateCache(path)
	if got.Latest != want.Latest || !got.CheckedAt.Equal(want.CheckedAt) {
		t.Fatalf("cache round-trip = %+v, want %+v", got, want)
	}
	// A missing file yields a zero cache, not a panic.
	if empty := readUpdateCache(filepath.Join(t.TempDir(), "nope.json")); empty.Latest != "" {
		t.Fatalf("expected empty cache for missing file, got %+v", empty)
	}
}
