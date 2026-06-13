package logs

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateRunLogPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	path, err := CreateRunLog(dir, "cx_123", "run_1", []byte("jsonl\n"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %o, want 0600", got)
	}
	if filepath.Base(path) != "run_1.jsonl" {
		t.Fatalf("unexpected log path: %s", path)
	}
}

func TestOpenRunLogWriter(t *testing.T) {
	dir := t.TempDir()
	path, w, err := OpenRunLogWriter(dir, "cx_123", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "line\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "line\n" {
		t.Fatalf("unexpected log content: %q", got)
	}
}

func TestPruneRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	oldPath, err := CreateRunLog(dir, "cx_old", "run_1", []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	cutoffPath, err := CreateRunLog(dir, "cx_cutoff", "run_1", []byte("cutoff"))
	if err != nil {
		t.Fatal(err)
	}
	newPath, err := CreateRunLog(dir, "cx_new", "run_1", []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPath, now.Add(-15*24*time.Hour), now.Add(-15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cutoffPath, now.Add(-14*24*time.Hour), now.Add(-14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, now.Add(-13*24*time.Hour), now.Add(-13*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Prune(dir, 14, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old log should be pruned, stat err=%v", err)
	}
	for _, path := range []string{cutoffPath, newPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("log should be kept %s: %v", path, err)
		}
	}
}

func TestDefaultDir(t *testing.T) {
	got := DefaultDir("/home/alice")
	want := filepath.Join("/home/alice", ".codex-feishu-bridge", "logs")
	if got != want {
		t.Fatalf("DefaultDir() = %q, want %q", got, want)
	}
}
