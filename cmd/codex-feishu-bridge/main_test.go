package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

func TestDoctorCommandRendersReport(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(`
[feishu]
app_secret_env = "FEISHU_APP_SECRET"

[workspace]
default = "/missing"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runWithIO(context.Background(), []string{"doctor", "--config", configPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "FAIL feishu.app_id") {
		t.Fatalf("expected doctor report, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestInitConfigCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	code := runWithIO(context.Background(), []string{"init-config", "--config", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	code = runWithIO(context.Background(), []string{"init-config", "--config", path}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected overwrite refusal")
	}
}

func TestTasksListAndShowCommands(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(`
[feishu]
app_id = "cli_test"
app_secret_env = "FEISHU_APP_SECRET"

[workspace]
default = "`+workspace+`"

[paths]
state_db = "`+filepath.Join(dir, "state.db")+`"
log_dir = "`+filepath.Join(dir, "logs")+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	admit, err := st.AdmitNewTask(ctx, "evt", "message", store.CreateTaskInput{TaskID: "cx_123", RunID: "run_1", CWD: workspace, CreatedBy: "ou", ChatID: "chat", Prompt: "hello", EffectiveCodexCommand: "codex", EffectiveSandbox: "workspace-write", EffectiveApproval: "never", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, "evt", admit.Run.ID, contracts.RunResult{CodexSessionID: "thread", FinalText: "done", FinishedAt: now}, "succeeded"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEISHU_APP_SECRET", "secret")
	var stdout, stderr bytes.Buffer
	code := runWithIO(ctx, []string{"tasks", "list", "--config", configPath}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "cx_123") {
		t.Fatalf("list failed code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = runWithIO(ctx, []string{"tasks", "show", "--config", configPath, "cx_123"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "status succeeded") {
		t.Fatalf("show failed code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
