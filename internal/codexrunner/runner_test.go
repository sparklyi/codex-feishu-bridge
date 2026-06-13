package codexrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunnerExecBuildsCommandWritesPrivateLogAndCallsSessionCallback(t *testing.T) {
	fake := writeFakeCodex(t)
	logDir := t.TempDir()
	cwd := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_CODEX_ARGV", argvFile)
	t.Setenv("FAKE_CODEX_OUTPUT", filepath.Join("testdata", "success.jsonl"))
	var sessionCalls []string
	r := Runner{LogDir: logDir, Now: fixedNow}
	result, err := r.Exec(context.Background(), ExecInput{
		Command:     fake,
		CWD:         cwd,
		Sandbox:     "workspace-write",
		ExtraArgs:   []string{"--skip-git-repo-check"},
		Prompt:      "hello",
		TaskID:      "cx_123",
		RunID:       "run_1",
		OnSessionID: func(id string) error { sessionCalls = append(sessionCalls, id); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"exec", "--json", "-C", cwd, "-s", "workspace-write", "--skip-git-repo-check", "hello"}
	if got := readArgv(t, argvFile); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgs)
	}
	if result.CodexSessionID == "" || result.FinalText != "OK" || result.ExitCode != 0 || result.LogPath == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !reflect.DeepEqual(sessionCalls, []string{result.CodexSessionID}) {
		t.Fatalf("unexpected session callbacks: %+v", sessionCalls)
	}
	logBytes, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "success.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(logBytes) != string(fixture) {
		t.Fatalf("raw JSONL log changed:\n%s", string(logBytes))
	}
	info, err := os.Stat(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %o, want 0600", info.Mode().Perm())
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		t.Fatalf("bad timestamps: %+v", result)
	}
}

func TestRunnerResumeBuildsStoredSnapshotArgs(t *testing.T) {
	fake := writeFakeCodex(t)
	logDir := t.TempDir()
	cwd := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_CODEX_ARGV", argvFile)
	t.Setenv("FAKE_CODEX_OUTPUT", filepath.Join("testdata", "success.jsonl"))
	r := Runner{LogDir: logDir, Now: fixedNow}
	_, err := r.Resume(context.Background(), ResumeInput{
		ExecInput: ExecInput{
			Command:               fake,
			CWD:                   cwd,
			Sandbox:               "read-only",
			Model:                 "gpt-5",
			Approval:              "never",
			ApprovalFlagSupported: true,
			ExtraArgs:             []string{"--foo"},
			TaskID:                "cx_123",
			RunID:                 "run_2",
		},
		SessionID: "thread_1",
		Reply:     "continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"exec", "--json", "-C", cwd, "-s", "read-only", "-m", "gpt-5", "-a", "never", "--foo", "resume", "thread_1", "continue"}
	if got := readArgv(t, argvFile); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgs)
	}
}

func TestRunnerOmitModelAndUnsupportedApproval(t *testing.T) {
	fake := writeFakeCodex(t)
	argvFile := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_CODEX_ARGV", argvFile)
	t.Setenv("FAKE_CODEX_OUTPUT", filepath.Join("testdata", "success.jsonl"))
	r := Runner{LogDir: t.TempDir(), Now: fixedNow}
	_, err := r.Exec(context.Background(), ExecInput{
		Command:  fake,
		CWD:      t.TempDir(),
		Sandbox:  "workspace-write",
		Approval: "never",
		Prompt:   "hello",
		TaskID:   "cx",
		RunID:    "run",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(readArgv(t, argvFile), "\x00")
	for _, banned := range []string{"\x00-m\x00", "\x00-a\x00"} {
		if strings.Contains(got, banned) {
			t.Fatalf("unexpected arg %q in %q", banned, got)
		}
	}
}

func TestRunnerNonZeroExitKeepsParsedResultAndStderrTail(t *testing.T) {
	fake := writeFakeCodex(t)
	t.Setenv("FAKE_CODEX_OUTPUT", filepath.Join("testdata", "success.jsonl"))
	t.Setenv("FAKE_CODEX_EXIT", "7")
	t.Setenv("FAKE_CODEX_STDERR", strings.Repeat("x", 9000))
	r := Runner{LogDir: t.TempDir(), Now: fixedNow}
	result, err := r.Exec(context.Background(), ExecInput{Command: fake, CWD: t.TempDir(), Sandbox: "workspace-write", Prompt: "hello", TaskID: "cx", RunID: "run"})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Kind != "non_zero_exit" || runErr.ExitCode != 7 {
		t.Fatalf("expected non_zero_exit, result=%+v err=%v", result, err)
	}
	if result.FinalText != "OK" || result.ExitCode != 7 {
		t.Fatalf("expected parsed non-zero result, got %+v", result)
	}
	if len(result.StderrTail) != 8192 {
		t.Fatalf("stderr tail len = %d", len(result.StderrTail))
	}
}

func TestRunnerParseCommandNotFoundAndCancelErrors(t *testing.T) {
	r := Runner{LogDir: t.TempDir(), Now: fixedNow}
	_, err := r.Exec(context.Background(), ExecInput{Command: filepath.Join(t.TempDir(), "missing"), CWD: t.TempDir(), Sandbox: "workspace-write", Prompt: "hello", TaskID: "cx", RunID: "run"})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Kind != "command_not_found" {
		t.Fatalf("expected command_not_found, got %v", err)
	}

	fake := writeFakeCodex(t)
	t.Setenv("FAKE_CODEX_SLEEP", "2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = r.Exec(ctx, ExecInput{Command: fake, CWD: t.TempDir(), Sandbox: "workspace-write", Prompt: "hello", TaskID: "cx", RunID: "run_cancel"})
	if !errors.As(err, &runErr) || runErr.Kind != "canceled" {
		t.Fatalf("expected canceled, got %v", err)
	}

	t.Setenv("FAKE_CODEX_SLEEP", "")
	t.Setenv("FAKE_CODEX_OUTPUT", filepath.Join("testdata", "malformed.jsonl"))
	_, err = r.Exec(context.Background(), ExecInput{Command: fake, CWD: t.TempDir(), Sandbox: "workspace-write", Prompt: "hello", TaskID: "cx", RunID: "run_parse"})
	if !errors.As(err, &runErr) || runErr.Kind != "parse" {
		t.Fatalf("expected parse, got %v", err)
	}
}

func writeFakeCodex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
if [ -n "$FAKE_CODEX_ARGV" ]; then
  : > "$FAKE_CODEX_ARGV"
  for arg in "$@"; do
    printf '%s\n' "$arg" >> "$FAKE_CODEX_ARGV"
  done
fi
if [ -n "$FAKE_CODEX_SLEEP" ]; then
  sleep "$FAKE_CODEX_SLEEP"
fi
if [ -n "$FAKE_CODEX_STDERR" ]; then
  printf '%s' "$FAKE_CODEX_STDERR" >&2
fi
if [ -n "$FAKE_CODEX_OUTPUT" ]; then
  cat "$FAKE_CODEX_OUTPUT"
else
  printf '{"type":"thread.started","thread_id":"thread_1"}\n{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}\n'
fi
exit "${FAKE_CODEX_EXIT:-0}"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
}
