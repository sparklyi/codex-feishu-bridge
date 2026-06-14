package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/codexrunner"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

func TestServeStartupAndReceiverFlow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := writeAppConfig(t, dir, workspace)
	stale, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.AdmitNewTask(ctx, "stale_evt", "message", store.CreateTaskInput{TaskID: "cx_stale", RunID: "run_stale", CWD: workspace, CreatedBy: "ou_owner", ChatID: "chat", Prompt: "stale", EffectiveCodexCommand: "codex", EffectiveSandbox: "workspace-write", EffectiveApproval: "never", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	oldLog := filepath.Join(dir, "logs", "cx_old", "run.jsonl")
	if err := os.MkdirAll(filepath.Dir(oldLog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldLog, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldLog, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	receiver := &fakeReceiver{events: []contracts.InboundEvent{{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"}}}
	sender := &fakeSender{ids: []string{"msg_start", "msg_result"}}
	runner := &fakeRunner{result: contracts.RunResult{CodexSessionID: "thread_1", FinalText: "done", StartedAt: now, FinishedAt: now}}
	err = Serve(ctx, ServeOptions{
		ConfigPath: configPath,
		Getenv: func(key string) string {
			if key == "FEISHU_APP_SECRET" {
				return "secret"
			}
			if key == "HOME" {
				return dir
			}
			return ""
		},
		Receiver: receiver,
		Sender:   sender,
		Runner:   runner,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if receiver.calls != 1 || runner.execCalls != 1 || len(sender.messages) != 2 {
		t.Fatalf("serve did not wire receiver/router: receiver=%d runner=%d messages=%d", receiver.calls, runner.execCalls, len(sender.messages))
	}
	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Fatalf("old log should be pruned, err=%v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ok, err := st.UserEnabled(ctx, "ou_owner")
	if err != nil || !ok {
		t.Fatalf("configured user not refreshed, ok=%v err=%v", ok, err)
	}
	tasks, err := st.ListTasks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundSession := false
	for _, task := range tasks {
		if task.CodexSessionID == "thread_1" && task.Status == "succeeded" {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatalf("new task with session not found: %+v", tasks)
	}
	staleTask, _, err := st.GetTask(ctx, "cx_stale")
	if err != nil {
		t.Fatal(err)
	}
	if staleTask.Status != "failed" {
		t.Fatalf("stale running task not recovered: %+v", staleTask)
	}
}

func TestInitConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := InitConfig(path, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "feishu:") || !strings.Contains(string(data), "FEISHU_APP_SECRET") {
		t.Fatalf("unexpected config:\n%s", string(data))
	}
	if !strings.Contains(string(data), "--ignore-user-config") {
		t.Fatalf("generated config should isolate bridge runs from stale user Codex project config:\n%s", string(data))
	}
	if err := InitConfig(path, false); err == nil {
		t.Fatal("expected no-overwrite error")
	}
	if err := InitConfig(path, true); err != nil {
		t.Fatal(err)
	}
}

func writeAppConfig(t *testing.T, dir, workspace string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	content := `
feishu:
  app_id: cli_test
  app_secret_env: FEISHU_APP_SECRET
  bot_open_id: ou_bot
  connection: websocket
security:
  allowed_open_ids:
    - ou_owner
codex:
  command: codex
  sandbox: workspace-write
  approval: never
  log_retention_days: 14
workspace:
  default: "` + workspace + `"
projects:
  backend:
    cwd: "` + workspace + `"
paths:
  state_db: "` + filepath.Join(dir, "state.db") + `"
  log_dir: "` + filepath.Join(dir, "logs") + `"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeReceiver struct {
	events []contracts.InboundEvent
	calls  int
}

func (f *fakeReceiver) Receive(ctx context.Context, handle func(context.Context, contracts.InboundEvent) error) error {
	f.calls++
	for _, ev := range f.events {
		if err := handle(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

type fakeSender struct {
	ids       []string
	failAfter int
	messages  []contracts.OutboundMessage
}

func (f *fakeSender) Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error) {
	f.messages = append(f.messages, msg)
	if f.failAfter > 0 && len(f.messages) >= f.failAfter {
		return contracts.SentMessage{}, os.ErrPermission
	}
	id := "msg"
	if len(f.ids) > 0 {
		id = f.ids[0]
		f.ids = f.ids[1:]
	}
	return contracts.SentMessage{MessageID: id}, nil
}

type fakeRunner struct {
	result      contracts.RunResult
	execCalls   int
	resumeCalls int
}

func (f *fakeRunner) Exec(ctx context.Context, in codexrunner.ExecInput) (contracts.RunResult, error) {
	f.execCalls++
	if in.OnSessionID != nil {
		if err := in.OnSessionID(f.result.CodexSessionID); err != nil {
			return contracts.RunResult{}, err
		}
	}
	return f.result, nil
}

func (f *fakeRunner) Resume(ctx context.Context, in codexrunner.ResumeInput) (contracts.RunResult, error) {
	f.resumeCalls++
	if in.OnSessionID != nil && in.SessionID != "" {
		_ = in.OnSessionID(in.SessionID)
	}
	return f.result, nil
}
