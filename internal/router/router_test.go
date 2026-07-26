package router

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/runtime"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

func TestRouterCreatesQueuedTaskAndDispatchesRuntime(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "new", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "input", Text: "fix tests"})
	if err != nil {
		t.Fatal(err)
	}
	if len(controller.enqueues) != 1 || controller.enqueues[0].Run.Kind != "new" || controller.enqueues[0].Project.CWD != "/repo/default" {
		t.Fatalf("unexpected runtime dispatch: %+v", controller.enqueues)
	}
	task, runs, err := st.GetTask(ctx, "task-1")
	if err != nil || task.Status != "queued" || task.RootMessageID != "card-1" || len(runs) != 1 {
		t.Fatalf("unexpected task=%+v runs=%+v err=%v", task, runs, err)
	}
	if routed, err := st.ResolveMessageRoute(ctx, "card-1"); err != nil || routed.ID != task.ID {
		t.Fatalf("start card route missing: %+v err=%v", routed, err)
	}
	if len(notes.starts) != 1 || notes.starts[0].Status != "queued" {
		t.Fatalf("unexpected start card: %+v", notes.starts)
	}
}

func TestRouterListsAndAttachesDesktopThread(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	controller.threads = []appserver.Thread{{ID: "desktop-1", Name: "Fix login", Preview: "Investigate login", CWD: "/repo/backend", Status: appserver.ThreadStatus{Type: "idle"}}}
	if err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, DedupKey: "sessions", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "input", Text: "/sessions"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.threadSelections) != 1 || len(notes.threadSelections[0].Threads) != 1 {
		t.Fatalf("expected thread selection card: %+v", notes.threadSelections)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundCardAction, DedupKey: "attach", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "callback", ActionValue: map[string]string{"action": "attach_thread", "thread_id": "desktop-1"}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListTasks(ctx, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	task := tasks[0]
	if task.CodexThreadID != "desktop-1" || task.Status != "idle" || task.ProjectAlias != "backend" || task.RootMessageID == "" {
		t.Fatalf("unexpected attached task: %+v", task)
	}
	if len(notes.starts) != 1 || notes.starts[0].Status != "idle" {
		t.Fatalf("attached card missing: %+v", notes.starts)
	}
}

func TestRouterResumesAttachedThreadAndHandlesStop(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	now := time.Now().UTC()
	task, _, err := st.AttachThread(ctx, "attach", "message", store.AttachThreadInput{TaskID: "attached", ThreadID: "desktop-thread", ProjectAlias: "backend", CWD: "/repo/backend", CreatedBy: "ou_owner", ChatID: "chat", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskRootMessageID(ctx, task.ID, "task-card", now); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertMessageRoute(ctx, "task-card", task.ID, "start_card"); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundReply, DedupKey: "resume", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "reply", RootMessageID: "task-card", Text: "continue from desktop"}); err != nil {
		t.Fatal(err)
	}
	if len(controller.enqueues) != 1 || controller.enqueues[0].Run.Kind != "resume" || controller.enqueues[0].Task.CodexThreadID != "desktop-thread" || controller.enqueues[0].Project.CWD != "/repo/backend" {
		t.Fatalf("unexpected resume dispatch: %+v", controller.enqueues)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundCardAction, ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "stop", ActionValue: map[string]string{"action": "stop_task", "task_id": task.ID}}); err != nil {
		t.Fatal(err)
	}
	if len(controller.stops) != 1 || controller.stops[0] != task.ID {
		t.Fatalf("stop not routed: %+v", controller.stops)
	}
	if len(notes.rejections) != 0 {
		t.Fatalf("unexpected rejections: %+v", notes.rejections)
	}
}

func TestRouterSteersActiveTaskWithoutCreatingAnotherRun(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	admit, err := st.AdmitNewTask(ctx, "new", "message", store.CreateTaskInput{
		TaskID: "running", RunID: "running-run", CWD: "/repo/default", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "work", Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.StartRun(ctx, store.StartRunInput{RunID: admit.Run.ID, ThreadID: "thread-1", TurnID: "turn-1", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "card", Text: "also verify the result",
		ActionValue: map[string]string{"action": "steer", "task_id": admit.Task.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(controller.steers) != 1 || controller.steers[0] != (steerCall{TaskID: admit.Task.ID, Text: "also verify the result"}) {
		t.Fatalf("steer was not routed to the active task: %+v", controller.steers)
	}
	_, runs, err := st.GetTask(ctx, admit.Task.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != admit.Run.ID || len(controller.enqueues) != 0 || len(notes.rejections) != 0 {
		t.Fatalf("steer should not create a run: runs=%+v enqueues=%+v rejections=%+v err=%v", runs, controller.enqueues, notes.rejections, err)
	}
}

func TestRouterSendsAndPatchesDetailsCard(t *testing.T) {
	ctx := context.Background()
	router, st, _, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	task := newFinishedTask(t, ctx, st)
	if err := router.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "result-card", RootMessageID: "result-card",
		ActionValue: map[string]string{"action": "view_details", "task_id": task.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(notes.details) != 1 || notes.details[0].ReplyToMessageID != "result-card" || notes.details[0].UpdateMessageID != "" || notes.details[0].FinalText != "final result" {
		t.Fatalf("details card was not sent from the persisted result: %+v", notes.details)
	}
	if routed, err := st.ResolveMessageRoute(ctx, "details"); err != nil || routed.ID != task.ID {
		t.Fatalf("details route missing: task=%+v err=%v", routed, err)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "details",
		ActionValue: map[string]string{"action": "details_page", "task_id": task.ID, "page": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(notes.details) != 2 || notes.details[1].UpdateMessageID != "details" || notes.details[1].Page != 1 {
		t.Fatalf("details page should patch its existing card: %+v", notes.details)
	}
}

func TestRouterRejectsSteerAndDetailsOutsideTaskOwnership(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	task, _, err := st.AttachThread(ctx, "attach", "message", store.AttachThreadInput{
		TaskID: "owned", ThreadID: "thread-1", CWD: "/repo/default", CreatedBy: "ou_owner", ChatID: "chat", Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, ChatType: "private", ChatID: "chat", SenderOpenID: "ou_other", MessageID: "card", Text: "change direction",
		ActionValue: map[string]string{"action": "steer", "task_id": task.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{
		Kind: contracts.InboundCardAction, ChatType: "private", ChatID: "other-chat", SenderOpenID: "ou_owner", MessageID: "card",
		ActionValue: map[string]string{"action": "view_details", "task_id": task.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(controller.steers) != 0 || len(notes.details) != 0 || len(notes.rejections) != 2 {
		t.Fatalf("out-of-scope actions must be rejected: steers=%+v details=%+v rejections=%+v", controller.steers, notes.details, notes.rejections)
	}
}

func TestRouterRejectsUnauthorizedAndRouteMiss(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	if err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundNewTask, ChatType: "private", ChatID: "chat", SenderOpenID: "ou_bad", MessageID: "input", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.rejections) != 1 || len(controller.enqueues) != 0 {
		t.Fatalf("unauthorized request should not dispatch: notes=%+v controller=%+v", notes.rejections, controller.enqueues)
	}
	if err := router.Handle(ctx, contracts.InboundEvent{Kind: contracts.InboundReply, DedupKey: "miss", ChatType: "private", ChatID: "chat", SenderOpenID: "ou_owner", MessageID: "reply", RootMessageID: "missing", Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.routingErrors) != 1 {
		t.Fatalf("route miss card missing: %+v", notes.routingErrors)
	}
}

func TestRouterIgnoresNonPrivateEventsBeforeAuthorization(t *testing.T) {
	ctx := context.Background()
	router, st, controller, notes := newTestRouter(t)
	defer func() { _ = st.Close() }()
	err := router.Handle(ctx, contracts.InboundEvent{
		Kind:         contracts.InboundNewTask,
		DedupKey:     "non-private",
		ChatType:     "non_private",
		ChatID:       "non-private-chat",
		SenderOpenID: "ou_bad",
		MessageID:    "input",
		Text:         "@backend fix tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(controller.enqueues) != 0 || len(notes.rejections) != 0 || len(notes.starts) != 0 {
		t.Fatalf("non-private event must be ignored: controller=%+v notes=%+v", controller, notes)
	}
}

func newTestRouter(t *testing.T) (*Router, *store.Store, *fakeController, *fakeNotifier) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RefreshUsers(context.Background(), []string{"ou_owner", "ou_other"}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	controller := &fakeController{}
	notes := &fakeNotifier{startIDs: []string{"card-1", "card-2", "card-3"}}
	count := 0
	nextTask := func() string { count++; return "task-" + string(rune('0'+count)) }
	router := New(RouterOptions{
		Config: config.Config{
			Security:  config.SecurityConfig{AllowedOpenIDs: []string{"ou_owner", "ou_other"}},
			AppServer: config.AppServerConfig{},
			Workspace: config.WorkspaceConfig{Default: "/repo/default"},
			Projects:  map[string]config.ProjectConfig{"backend": {CWD: "/repo/backend"}},
		},
		Store:      st,
		Controller: controller,
		Notifier:   notes,
		Now:        func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
		NewTaskID:  nextTask,
		NewRunID:   func() string { return "run-1" },
	})
	return router, st, controller, notes
}

type fakeController struct {
	threads  []appserver.Thread
	enqueues []runtime.StartInput
	steers   []steerCall
	stops    []string
	err      error
}

func (f *fakeController) Threads(context.Context, int) ([]appserver.Thread, error) {
	return append([]appserver.Thread(nil), f.threads...), f.err
}
func (f *fakeController) Enqueue(_ context.Context, input runtime.StartInput) error {
	f.enqueues = append(f.enqueues, input)
	return f.err
}
func (f *fakeController) Steer(_ context.Context, taskID, text string) error {
	f.steers = append(f.steers, steerCall{TaskID: taskID, Text: text})
	return f.err
}

type steerCall struct {
	TaskID string
	Text   string
}

func newFinishedTask(t *testing.T, ctx context.Context, st *store.Store) store.Task {
	t.Helper()
	admit, err := st.AdmitNewTask(ctx, "finished", "message", store.CreateTaskInput{
		TaskID: "finished", RunID: "finished-run", CWD: "/repo/default", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "work", Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.StartRun(ctx, store.StartRunInput{RunID: admit.Run.ID, ThreadID: "thread-1", TurnID: "turn-1", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, "finished", store.FinishRunInput{
		RunID: admit.Run.ID, ThreadID: "thread-1", TurnID: "turn-1", Status: "succeeded", ExitCode: 0, FinalText: "final result", FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	task, _, err := st.GetTask(ctx, admit.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func (f *fakeController) Stop(_ context.Context, taskID string) error {
	f.stops = append(f.stops, taskID)
	return f.err
}

type fakeNotifier struct {
	startIDs         []string
	starts           []notifier.TaskCardInput
	details          []notifier.DetailsInput
	threadSelections []notifier.ThreadSelectionInput
	routingErrors    []string
	rejections       []string
}

func (f *fakeNotifier) Start(_ context.Context, input notifier.TaskCardInput) (contracts.SentMessage, error) {
	f.starts = append(f.starts, input)
	id := "card"
	if len(f.startIDs) > 0 {
		id = f.startIDs[0]
		f.startIDs = f.startIDs[1:]
	}
	return contracts.SentMessage{MessageID: id}, nil
}
func (f *fakeNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "failure"}, nil
}
func (f *fakeNotifier) Details(_ context.Context, input notifier.DetailsInput) (contracts.SentMessage, error) {
	f.details = append(f.details, input)
	return contracts.SentMessage{MessageID: "details"}, nil
}
func (f *fakeNotifier) ThreadSelection(_ context.Context, input notifier.ThreadSelectionInput) (contracts.SentMessage, error) {
	f.threadSelections = append(f.threadSelections, input)
	return contracts.SentMessage{MessageID: "threads"}, nil
}
func (f *fakeNotifier) RoutingError(_ context.Context, _ string, replyTo string) (contracts.SentMessage, error) {
	f.routingErrors = append(f.routingErrors, replyTo)
	return contracts.SentMessage{MessageID: "route"}, nil
}
func (f *fakeNotifier) Rejection(_ context.Context, _ string, _ string, body string) error {
	f.rejections = append(f.rejections, body)
	return nil
}
func (f *fakeNotifier) RunningConflict(context.Context, notifier.RunningConflictInput) error {
	return nil
}

var _ Controller = (*fakeController)(nil)
var _ Notifier = (*fakeNotifier)(nil)
