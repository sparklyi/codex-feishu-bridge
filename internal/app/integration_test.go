package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

func TestIntegration(t *testing.T) {
	t.Run("new task", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"}})
		env.run(t)
		if env.runner.execCalls != 1 || len(env.sender.messages) != 2 {
			t.Fatalf("unexpected flow exec=%d messages=%d", env.runner.execCalls, len(env.sender.messages))
		}
	})

	t.Run("private plain task", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{{Kind: contracts.InboundNewTask, DedupKey: "plain_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "fix the failing router test"}})
		env.run(t)
		if env.runner.execCalls != 1 || env.sender.messages[0].CardKind != contracts.CardStart {
			t.Fatalf("private plain task did not start exec=%d messages=%+v", env.runner.execCalls, env.sender.messages)
		}
	})

	t.Run("card reply resume", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{
			{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"},
			{Kind: contracts.InboundCardAction, DedupKey: "evt_2", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb", RootMessageID: "msg_result", ActionID: "continue_submit", Text: "continue"},
		})
		env.run(t)
		if env.runner.resumeCalls != 1 {
			t.Fatalf("resume calls = %d", env.runner.resumeCalls)
		}
	})

	t.Run("group mention project selection then task", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{{
			Kind: contracts.InboundNewTask, DedupKey: "group_1", ChatType: "group",
			ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user",
			Text: "fix tests", BotMentioned: true,
		}})
		env.run(t)
		if env.runner.execCalls != 0 || len(env.sender.messages) != 1 || env.sender.messages[0].CardKind != contracts.CardProjectSelection {
			t.Fatalf("expected project selection exec=%d messages=%+v", env.runner.execCalls, env.sender.messages)
		}
		pendingID := env.sender.messages[0].Actions[0].Value["pending_id"]
		env.receiver.events = []contracts.InboundEvent{{
			Kind: contracts.InboundCardAction, DedupKey: "group_2", ChatType: "group",
			ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb",
			ActionID:    "project_select",
			ActionValue: map[string]string{"action": "select_project", "pending_id": pendingID, "project": "backend"},
		}}
		env.run(t)
		if env.runner.execCalls != 1 {
			t.Fatalf("project selection did not start task, exec=%d messages=%+v", env.runner.execCalls, env.sender.messages)
		}
	})

	t.Run("route miss never falls back to latest task", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{
			{Kind: contracts.InboundNewTask, DedupKey: "miss_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"},
			{Kind: contracts.InboundReply, DedupKey: "miss_2", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_reply", RootMessageID: "missing", Text: "continue"},
		})
		env.run(t)
		if env.runner.resumeCalls != 0 || env.sender.messages[len(env.sender.messages)-1].CardKind != contracts.CardRoutingError {
			t.Fatalf("route miss should not resume latest task resumes=%d messages=%+v", env.runner.resumeCalls, env.sender.messages)
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{{Kind: contracts.InboundNewTask, DedupKey: "evt_bad", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_bad", MessageID: "msg_user", Text: "hello"}})
		env.run(t)
		if env.runner.execCalls != 0 || len(env.sender.messages) != 1 {
			t.Fatalf("unauthorized should reject without runner, exec=%d messages=%d", env.runner.execCalls, len(env.sender.messages))
		}
	})

	t.Run("duplicate event", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{
			{Kind: contracts.InboundNewTask, DedupKey: "evt_dup", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"},
			{Kind: contracts.InboundNewTask, DedupKey: "evt_dup", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user2", Text: "hello again"},
		})
		env.run(t)
		if env.runner.execCalls != 1 {
			t.Fatalf("duplicate should run once, exec=%d", env.runner.execCalls)
		}
	})

	t.Run("result card failure", func(t *testing.T) {
		env := newIntegrationEnv(t, []contracts.InboundEvent{{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "hello"}})
		env.sender.failAfter = 2
		env.run(t)
		st := env.openStore(t)
		defer st.Close()
		tasks, err := st.ListTasks(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 || tasks[0].Status != "succeeded" {
			t.Fatalf("result send failure should preserve run result: %+v", tasks)
		}
	})

	t.Run("restart recovery", func(t *testing.T) {
		env := newIntegrationEnv(t, nil)
		st := env.openStore(t)
		now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
		if _, err := st.AdmitNewTask(context.Background(), "stale", "message", store.CreateTaskInput{TaskID: "cx_stale", RunID: "run_stale", CWD: env.workspace, CreatedBy: "ou_owner", ChatID: "chat", Prompt: "stale", EffectiveCodexCommand: "codex", EffectiveSandbox: "workspace-write", EffectiveApproval: "never", Now: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		env.run(t)
		st = env.openStore(t)
		defer st.Close()
		task, _, err := st.GetTask(context.Background(), "cx_stale")
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != "failed" {
			t.Fatalf("stale task not recovered: %+v", task)
		}
	})
}

type integrationEnv struct {
	dir        string
	workspace  string
	configPath string
	receiver   *fakeReceiver
	sender     *fakeSender
	runner     *fakeRunner
}

func newIntegrationEnv(t *testing.T, events []contracts.InboundEvent) *integrationEnv {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return &integrationEnv{
		dir:        dir,
		workspace:  workspace,
		configPath: writeAppConfig(t, dir, workspace),
		receiver:   &fakeReceiver{events: events},
		sender:     &fakeSender{ids: []string{"msg_start", "msg_result", "msg_resume_start", "msg_resume_result"}},
		runner:     &fakeRunner{result: contracts.RunResult{CodexSessionID: "thread_1", FinalText: "done", StartedAt: time.Now(), FinishedAt: time.Now()}},
	}
}

func (e *integrationEnv) run(t *testing.T) {
	t.Helper()
	err := Serve(context.Background(), ServeOptions{
		ConfigPath: e.configPath,
		Getenv: func(key string) string {
			if key == "FEISHU_APP_SECRET" {
				return "secret"
			}
			if key == "HOME" {
				return e.dir
			}
			return ""
		},
		Receiver: e.receiver,
		Sender:   e.sender,
		Runner:   e.runner,
		Now:      func() time.Time { return time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *integrationEnv) openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(e.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
