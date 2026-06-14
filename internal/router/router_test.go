package router

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/codexrunner"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

func TestRouterNewTaskRecordsRoutesAndSessionBeforeFinish(t *testing.T) {
	ctx := context.Background()
	rt, st, runner, notes := newTestRouter(t, []string{"ou_owner"})
	runner.onSession = func() {
		task, _, err := st.GetTask(ctx, "cx_1")
		if err != nil {
			t.Fatal(err)
		}
		if task.CodexSessionID != "thread_1" || task.Status != "running" {
			t.Fatalf("session should be persisted before finish, task=%+v", task)
		}
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 1 {
		t.Fatalf("runner exec calls = %d", runner.execCalls)
	}
	for _, messageID := range []string{"msg_start", "msg_result"} {
		task, err := st.ResolveMessageRoute(ctx, messageID)
		if err != nil {
			t.Fatalf("route %s missing: %v", messageID, err)
		}
		if task.ID != "cx_1" {
			t.Fatalf("unexpected routed task: %+v", task)
		}
	}
	if len(notes.starts) != 1 || len(notes.successes) != 1 {
		t.Fatalf("unexpected notifier calls: %+v", notes)
	}
}

func TestRouterReplyResumesCreatorOnlyAndRejectsRouteMiss(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouter(t, []string{"ou_owner", "ou_other"})
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_user", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundReply, DedupKey: "evt_2", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_reply", RootMessageID: "msg_result", Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	if runner.resumeCalls != 1 {
		t.Fatalf("resume calls = %d", runner.resumeCalls)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundReply, DedupKey: "evt_3", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_other", MessageID: "msg_reply2", RootMessageID: "msg_result", Text: "steal"}); err != nil {
		t.Fatal(err)
	}
	if runner.resumeCalls != 1 || len(notes.rejections) == 0 {
		t.Fatalf("creator-only rejection failed, resumes=%d notes=%+v", runner.resumeCalls, notes)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundReply, DedupKey: "evt_4", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg_reply3", RootMessageID: "missing", Text: "lost"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.routingErrors) != 1 {
		t.Fatalf("expected routing error, got %+v", notes.routingErrors)
	}
}

func TestRouterAuthorizationDuplicateAndStartFailure(t *testing.T) {
	ctx := context.Background()
	rt, st, runner, notes := newTestRouter(t, []string{"ou_owner"})
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "unauth_private", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_bad", MessageID: "msg", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.rejections) != 1 || runner.execCalls != 0 {
		t.Fatalf("private unauthorized should reject without runner: notes=%+v exec=%d", notes, runner.execCalls)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "unauth_group", ChatType: "group", ChatID: "chat", SenderOpenID: "ou_bad", MessageID: "msg", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.rejections) != 1 || runner.execCalls != 0 {
		t.Fatalf("group unauthorized should be silent: notes=%+v exec=%d", notes, runner.execCalls)
	}

	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg", Text: "/codex hello again"}); err != nil {
		t.Fatal(err)
	}
	if runner.execCalls != 1 {
		t.Fatalf("duplicate event should not rerun codex, exec=%d", runner.execCalls)
	}

	rt2, st2, runner2, notes2 := newTestRouter(t, []string{"ou_owner"})
	notes2.startErr = errors.New("send failed")
	if err := rt2.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_start_fail", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if runner2.execCalls != 0 {
		t.Fatalf("start send failure should not run codex")
	}
	task, runs, err := st2.GetTask(ctx, "cx_1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "failed" || runs[0].Status != "failed" {
		t.Fatalf("start failure should fail task/run: task=%+v runs=%+v", task, runs)
	}
	_ = st
}

func TestRouterCardActionEmptyTextRejectedBeforeRun(t *testing.T) {
	ctx := context.Background()
	rt, _, runner, notes := newTestRouter(t, []string{"ou_owner"})
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "evt_1", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "msg", Text: "/codex hello"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundCardAction, DedupKey: "evt_2", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card_cb", RootMessageID: "msg_result", ActionID: "continue_submit", Text: "   "}); err != nil {
		t.Fatal(err)
	}
	if runner.resumeCalls != 0 || len(notes.rejections) == 0 {
		t.Fatalf("empty action should reject before resume, resumes=%d notes=%+v", runner.resumeCalls, notes)
	}
}

func newTestRouter(t *testing.T, allowed []string) (*Router, *store.Store, *fakeRunner, *fakeNotifier) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.RefreshUsers(ctx, allowed); err != nil {
		t.Fatal(err)
	}
	notes := &fakeNotifier{
		startIDs:  []string{"msg_start", "msg_start_2", "msg_start_3"},
		resultIDs: []string{"msg_result", "msg_result_2", "msg_result_3"},
	}
	runner := &fakeRunner{result: contracts.RunResult{CodexSessionID: "thread_1", FinalText: "done", ExitCode: 0, StartedAt: time.Now(), FinishedAt: time.Now()}}
	cfg := config.Config{
		Security:  config.SecurityConfig{AllowedOpenIDs: allowed},
		Codex:     config.CodexConfig{Command: "codex", Sandbox: "workspace-write", Approval: "never"},
		Workspace: config.WorkspaceConfig{Default: t.TempDir()},
	}
	rt := New(RouterOptions{
		Config:   cfg,
		Store:    st,
		Runner:   runner,
		Notifier: notes,
		Now:      func() time.Time { return time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC) },
		NewTaskID: func() string {
			return "cx_1"
		},
		NewRunID: func() string {
			return "run_" + time.Now().Format("150405.000000000")
		},
	})
	return rt, st, runner, notes
}

type fakeRunner struct {
	result      contracts.RunResult
	execCalls   int
	resumeCalls int
	onSession   func()
}

func (f *fakeRunner) Exec(ctx context.Context, in codexrunner.ExecInput) (contracts.RunResult, error) {
	f.execCalls++
	if in.OnSessionID != nil {
		if err := in.OnSessionID("thread_1"); err != nil {
			return contracts.RunResult{}, err
		}
		if f.onSession != nil {
			f.onSession()
		}
	}
	return f.result, nil
}

func (f *fakeRunner) Resume(ctx context.Context, in codexrunner.ResumeInput) (contracts.RunResult, error) {
	f.resumeCalls++
	if in.SessionID == "" {
		return contracts.RunResult{}, errors.New("missing session")
	}
	if in.OnSessionID != nil {
		_ = in.OnSessionID(in.SessionID)
	}
	return f.result, nil
}

type fakeNotifier struct {
	startIDs      []string
	resultIDs     []string
	startErr      error
	starts        []notifier.TaskCardInput
	successes     []notifier.TaskCardInput
	failures      []notifier.TaskCardInput
	rejections    []string
	routingErrors []string
}

func (f *fakeNotifier) Start(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error) {
	f.starts = append(f.starts, in)
	if f.startErr != nil {
		return contracts.SentMessage{}, f.startErr
	}
	return contracts.SentMessage{MessageID: popID(&f.startIDs)}, nil
}

func (f *fakeNotifier) Success(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error) {
	f.successes = append(f.successes, in)
	return contracts.SentMessage{MessageID: popID(&f.resultIDs)}, nil
}

func (f *fakeNotifier) Failure(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error) {
	f.failures = append(f.failures, in)
	return contracts.SentMessage{MessageID: popID(&f.resultIDs)}, nil
}

func (f *fakeNotifier) RoutingError(ctx context.Context, chatID, replyToMessageID string) (contracts.SentMessage, error) {
	f.routingErrors = append(f.routingErrors, replyToMessageID)
	return contracts.SentMessage{MessageID: "msg_routing"}, nil
}

func (f *fakeNotifier) Rejection(ctx context.Context, chatID, replyToMessageID, body string) error {
	f.rejections = append(f.rejections, body)
	return nil
}

func popID(ids *[]string) string {
	if len(*ids) == 0 {
		return ""
	}
	id := (*ids)[0]
	*ids = (*ids)[1:]
	return id
}
