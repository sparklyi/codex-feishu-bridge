package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

func TestControllerUsesConfiguredOperationalValues(t *testing.T) {
	controller := New(ControllerOptions{
		ProgressUpdateInterval:       200 * time.Millisecond,
		ProgressUpdateAttemptTimeout: 13 * time.Millisecond,
		ProgressRetryDelay:           17 * time.Millisecond,
		NotificationTimeout:          19 * time.Millisecond,
		AppServerTimeout:             23 * time.Millisecond,
		TerminalRetryAttempts:        4,
		TerminalRetryDelay:           29 * time.Millisecond,
	})
	defer controller.Close()
	if controller.progressUpdateInterval != 200*time.Millisecond || controller.progressUpdateAttemptTimeout != 13*time.Millisecond || controller.progressRetryDelay != 17*time.Millisecond || controller.notificationTimeout != 19*time.Millisecond || controller.appServerTimeout != 23*time.Millisecond || controller.terminalRetryAttempts != 4 || controller.terminalRetryDelay != 29*time.Millisecond {
		t.Fatalf("configured controller values were not retained: %+v", controller)
	}
}

func TestControllerRunsTurnStreamsResultAndPersistsState(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &fakeNotifier{}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes, CardDisplayMode: "preview"})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	controller.mu.Lock()
	active := controller.byTask[task.ID]
	controller.mu.Unlock()
	if active == nil {
		t.Fatal("active run was not registered")
	}
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"partial "}`)}
	api.events <- appserver.Event{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":"final answer"}}`)}
	api.events <- appserver.Event{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "succeeded"
	})
	stored, runs, err := st.GetTask(ctx, task.ID)
	if err != nil || stored.CodexThreadID != "thread-1" || len(runs) != 1 || runs[0].CodexTurnID != "turn-1" || runs[0].FinalText != "final answer" {
		t.Fatalf("unexpected task=%+v runs=%+v err=%v", stored, runs, err)
	}
	waitUntil(t, func() bool { return notes.count("success") == 1 })
	if notes.count("progress") == 0 {
		t.Fatal("expected at least one running card update")
	}
	select {
	case <-active.ctx.Done():
	default:
		t.Fatal("completed run context was not canceled")
	}
}

func TestControllerCoalescesSlowProgressCardUpdates(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := newBlockingProgressNotifier()
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes, CardDisplayMode: "preview"})
	defer controller.Close()
	defer notes.release()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitFor(t, notes.firstProgress)

	controller.mu.Lock()
	active := controller.byTask[task.ID]
	controller.mu.Unlock()
	if active == nil {
		t.Fatal("active run was not registered")
	}
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"first processing detail fragment "}`)}
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"second processing detail fragment "}`)}
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"latest processing detail fragment is complete."}`)}
	waitUntil(t, func() bool { return strings.Contains(active.text(), "latest processing detail fragment") })

	// Keep the first Patch in flight longer than one update interval. The stream
	// must collapse to one latest processing-detail update instead of replaying each delta.
	time.Sleep(progressUpdateInterval + 50*time.Millisecond)
	notes.release()
	waitUntil(t, func() bool { return notes.progressCount() >= 2 })
	updates := notes.progressUpdates()
	if len(updates) != 2 {
		t.Fatalf("progress updates = %d, want 2: %+v", len(updates), updates)
	}
	if !strings.Contains(updates[1].Presentation.ProcessingDetail, "latest processing detail fragment") {
		t.Fatalf("latest processing detail was not sent: %+v", updates[1].Presentation)
	}
	time.Sleep(progressUpdateInterval + 50*time.Millisecond)
	if count := notes.progressCount(); count != 2 {
		t.Fatalf("stale progress updates should not queue, got %d", count)
	}
}

func TestControllerRetriesTransientProgressPatch(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &transientProgressNotifier{needle: "retry-this-progress"}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes, CardDisplayMode: "preview"})
	defer controller.Close()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "turn-1") != nil })
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"retry-this-progress contains enough text to update the processing detail."}`)}

	waitUntilFor(t, 4*time.Second, func() bool { return notes.matchCount() == 2 })
	for _, update := range notes.matchingUpdates() {
		if update.UpdateMessageID != "card" {
			t.Fatalf("retry must patch the original card: %+v", update)
		}
	}
}

func TestControllerQueuesShortStreamDeltaForProgressPatch(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &recordingNotifier{}
	controller := New(ControllerOptions{
		AppServer:              api,
		Store:                  st,
		Notifier:               notes,
		CardDisplayMode:        "preview",
		ProgressUpdateInterval: time.Millisecond,
	})
	defer controller.Close()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "turn-1") != nil })
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"ok"}`)}

	waitUntilFor(t, time.Second, func() bool {
		return notes.hasProgress(func(input notifier.TaskCardInput) bool {
			return input.Presentation.ProcessingDetail == "ok"
		})
	})
}

func TestControllerRetriesProgressAfterShortAttemptTimeout(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := newDeadlineProgressNotifier()
	controller := New(ControllerOptions{
		AppServer:                    api,
		Store:                        st,
		Notifier:                     notes,
		CardDisplayMode:              "preview",
		ProgressUpdateAttemptTimeout: 75 * time.Millisecond,
		ProgressRetryDelay:           25 * time.Millisecond,
		NotificationTimeout:          time.Second,
	})
	defer controller.Close()

	started := time.Now()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, notes.firstProgress)
	waitFor(t, notes.secondProgress)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("progress retry waited too long: %s", elapsed)
	}
	if elapsed := notes.firstAttemptElapsed(); elapsed > 300*time.Millisecond {
		t.Fatalf("progress attempt ignored its short timeout: %s", elapsed)
	}
	if notes.progressCount() < 2 {
		t.Fatalf("progress attempts = %d, want at least 2", notes.progressCount())
	}
}

func TestControllerUsesItemEventsAndKeepsConciseCardsFreeOfProcessingDetails(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &recordingNotifier{}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes, CardDisplayMode: "concise"})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return notes.progressCount() > 0 })
	controller.mu.Lock()
	active := controller.byTask[task.ID]
	controller.mu.Unlock()
	if active == nil {
		t.Fatal("active run missing")
	}
	api.events <- appserver.Event{Method: "item/started", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","command":"go test ./..."}}`)}
	api.events <- appserver.Event{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","command":"go test ./...","exitCode":0}}`)}
	api.events <- appserver.Event{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"fileChange","changes":[{"path":"internal/runtime/controller.go"},{"path":"internal/router/router.go"}]}}`)}
	waitUntil(t, func() bool {
		presentation := active.progressPresentation()
		return len(presentation.Milestones) >= 2 && len(active.resultPresentation("succeeded", "").Changes) > 0
	})
	presentation := active.progressPresentation()
	if presentation.ProcessingDetail != "" || strings.Contains(presentation.Stage+presentation.Activity+milestoneText(presentation.Milestones), "go test") {
		t.Fatalf("concise presentation leaked processing detail or raw command: %+v", presentation)
	}
	waitUntil(t, func() bool {
		return notes.hasProgress(func(input notifier.TaskCardInput) bool {
			return len(input.Presentation.Milestones) >= 2
		})
	})
	before := notes.progressCount()
	api.events <- appserver.Event{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"this processing detail must stay local in concise mode."}`)}
	time.Sleep(progressUpdateInterval + 100*time.Millisecond)
	if after := notes.progressCount(); after != before {
		t.Fatalf("concise mode patched card for a delta: before=%d after=%d", before, after)
	}
}

func TestControllerSteersSameActiveTurn(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &recordingNotifier{}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool {
		if controller.activeFor("thread-1", "turn-1") == nil {
			return false
		}
		_, runs, err := st.GetTask(ctx, task.ID)
		return err == nil && len(runs) == 1 && runs[0].Status == "running" && runs[0].CodexTurnID == "turn-1"
	})
	if err := controller.Steer(ctx, task.ID, "also verify the test result"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		return notes.hasProgress(func(input notifier.TaskCardInput) bool {
			return len(input.Presentation.UserInputs) == 2 && input.Presentation.UserInputs[0] == "work" && input.Presentation.UserInputs[1] == "also verify the test result"
		})
	})
	steers := api.steers()
	if len(steers) != 1 || steers[0].ThreadID != "thread-1" || steers[0].ExpectedTurnID != "turn-1" || steers[0].Text != "also verify the test result" {
		t.Fatalf("steer did not target the active turn: %+v", steers)
	}
	_, runs, err := st.GetTask(ctx, task.ID)
	if err != nil || len(runs) != 1 || runs[0].Kind != "new" {
		t.Fatalf("steer should not create a new run: runs=%+v err=%v", runs, err)
	}
}

func TestActiveRunCarriesInputsToResultPresentation(t *testing.T) {
	active := &activeRun{userInputs: []string{"first instruction", "follow-up instruction"}, finalText: "done"}
	if got := active.progressPresentation().UserInputs; len(got) != 2 || got[0] != "first instruction" || got[1] != "follow-up instruction" {
		t.Fatalf("running presentation inputs = %+v", got)
	}
	if got := active.resultPresentation("succeeded", "").UserInputs; len(got) != 2 || got[0] != "first instruction" || got[1] != "follow-up instruction" {
		t.Fatalf("result presentation inputs = %+v", got)
	}
}

func TestClassifyDisplayItemOmitsCommandAndOutputDetails(t *testing.T) {
	started, ok := classifyDisplayItem(appServerItem{Type: "commandExecution", Command: "go test ./..."}, false)
	if !ok || started.stage != "验证" || started.activity != "正在执行测试。" || strings.Contains(started.activity, "go test") {
		t.Fatalf("unexpected started display item: %+v", started)
	}
	completed, ok := classifyDisplayItem(appServerItem{Type: "commandExecution", Command: "go test ./...", ExitCode: intPtr(0)}, true)
	if !ok || completed.milestone != "已完成测试" || completed.verification != "测试已完成" {
		t.Fatalf("unexpected completed display item: %+v", completed)
	}
	if _, ok := classifyDisplayItem(appServerItem{Type: "reasoning"}, false); ok {
		t.Fatal("reasoning must not be rendered")
	}
	failed, ok := classifyDisplayItem(appServerItem{Type: "commandExecution", Command: "go test ./...", Status: "failed"}, true)
	if !ok || failed.verification != "测试发现问题" {
		t.Fatalf("item status should mark a command failure: %+v", failed)
	}
}

func TestActiveRunUsesCompletedAgentMessageAsAuthoritativeText(t *testing.T) {
	active := &activeRun{processingDetail: "stale streamed processing detail"}
	active.setFinalText("authoritative final reply")
	if got := active.text(); got != "authoritative final reply" {
		t.Fatalf("completed agent message must replace processing detail, got %q", got)
	}
}

func TestActiveRunRetainsStreamedProcessingDetailWhenCompletionHasNoText(t *testing.T) {
	active := &activeRun{processingDetail: "streamed final reply"}
	active.setFinalText("")
	if got := active.text(); got != "streamed final reply" {
		t.Fatalf("empty completed item must retain the streamed reply, got %q", got)
	}
	if got := active.resultPresentation("succeeded", "").Conclusion; got != "streamed final reply" {
		t.Fatalf("result card conclusion lost the streamed reply, got %q", got)
	}
}

func TestTrimProcessingDetailKeepsAppendOnlyPrefix(t *testing.T) {
	value := strings.Repeat("处理详情", 4000)
	first := trimProcessingDetail(value)
	if len(first) > processingDetailLimit {
		t.Fatalf("processing detail exceeded limit: %d", len(first))
	}
	if next := trimProcessingDetail(value + "继续"); !strings.HasPrefix(next, first) {
		t.Fatalf("processing detail must remain append-only: first=%q next=%q", first, next)
	}
}

func TestResultPresentationUsesFinalReplyAsConclusion(t *testing.T) {
	finalReply := "## Summary\n完成任务卡改造。\n\n## Changes\n- 删除重复结果区块\n\n## 验证\n- go test ./... 已通过"
	presentation := (&activeRun{finalText: finalReply}).resultPresentation("succeeded", "")
	if presentation.Conclusion != finalReply {
		t.Fatalf("conclusion should retain the final reply, got %q", presentation.Conclusion)
	}
	if len(presentation.Changes) != 1 || presentation.Changes[0] != "删除重复结果区块" {
		t.Fatalf("changes = %+v", presentation.Changes)
	}
	if len(presentation.Verification) != 1 || presentation.Verification[0] != "go test ./... 已通过" {
		t.Fatalf("verification = %+v", presentation.Verification)
	}
}

func TestExtractResultDetailsUsesStructuredDeveloperSections(t *testing.T) {
	changes, verification := extractResultDetails("## Summary\n完成任务卡改造。\n\n## Changes\n- 新增事件归类\n\n## 验证\n- go test ./... 已通过")
	if len(changes) != 1 || changes[0] != "新增事件归类" {
		t.Fatalf("changes = %+v", changes)
	}
	if len(verification) != 1 || verification[0] != "go test ./... 已通过" {
		t.Fatalf("verification = %+v", verification)
	}
}

func TestExtractResultDetailsKeepsMarkdownInChangeItems(t *testing.T) {
	changes, _ := extractResultDetails("## Changes\n- **高优先级**：完成卡片没有最终回复")
	if len(changes) != 1 || changes[0] != "**高优先级**：完成卡片没有最终回复" {
		t.Fatalf("changes = %+v", changes)
	}
}

func milestoneText(values []contracts.TaskMilestone) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, value.Label)
	}
	return strings.Join(parts, "\n")
}

func intPtr(value int) *int { return &value }

func TestControllerKeepsOriginalTerminalCardForTransientPatchFailure(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &terminalPatchNotifier{patchErr: temporaryNotifierError{}, patchFailures: 1}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes, TerminalRetryDelay: time.Millisecond})
	defer controller.Close()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "turn-1") != nil })
	api.events <- appserver.Event{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}

	waitUntil(t, func() bool { return notes.successCount() == 2 })
	updates := notes.successUpdates()
	if len(updates) != 2 || updates[0].UpdateMessageID != "card" || updates[1].UpdateMessageID != "card" {
		t.Fatalf("transient terminal failure should keep the original card: %+v", updates)
	}
}

func TestControllerCreatesFallbackAfterTransientTerminalPatchRetriesAreExhausted(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &terminalPatchNotifier{patchErr: temporaryNotifierError{}, patchFailures: terminalRetryAttempts + 1}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes, TerminalRetryDelay: time.Millisecond})
	defer controller.Close()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "turn-1") != nil })
	api.events <- appserver.Event{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}

	wantCalls := terminalRetryAttempts + 2 // Initial patch, retries, then the fallback card.
	waitUntil(t, func() bool { return notes.successCount() == wantCalls })
	updates := notes.successUpdates()
	if updates[wantCalls-1].UpdateMessageID != "" {
		t.Fatalf("exhausted transient retries should create one fallback card: %+v", updates)
	}
}

func TestControllerCreatesFallbackForPermanentTerminalPatchFailure(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &terminalPatchNotifier{patchErr: errors.New("message cannot be updated"), patchFailures: 1}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes})
	defer controller.Close()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "turn-1") != nil })
	api.events <- appserver.Event{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}

	waitUntil(t, func() bool { return notes.successCount() == 2 })
	updates := notes.successUpdates()
	if len(updates) != 2 || updates[0].UpdateMessageID != "card" || updates[1].UpdateMessageID != "" {
		t.Fatalf("permanent terminal failure should create one fallback card: %+v", updates)
	}
}

func TestControllerAutomaticallyApprovesUnattendedRequests(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &fakeNotifier{}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "running"
	})
	api.requests <- appserver.ServerRequest{ID: json.RawMessage(`100`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item","command":"go test ./..."}`)}
	waitUntil(t, func() bool { return len(api.responses()) == 1 })
	response := api.responses()[0]
	if response.methodID != "100" || response.decision != "accept" {
		t.Fatalf("unexpected unattended approval response: %+v", response)
	}
	stored, runs, err := st.GetTask(ctx, task.ID)
	if err != nil || stored.Status != "running" || runs[0].Status != "running" {
		t.Fatalf("unattended approval should keep the turn running: task=%+v runs=%+v err=%v", stored, runs, err)
	}
}

func TestControllerStopsActiveTurn(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := &fakeNotifier{}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	if err := controller.Stop(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "canceled"
	})
	if api.interrupts() != 1 {
		t.Fatalf("interrupt count = %d", api.interrupts())
	}
}

func TestControllerStopDoesNotBlockOnInterrupt(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	releaseInterrupt := make(chan struct{})
	interruptStarted := make(chan struct{}, 1)
	api.interruptFn = func(context.Context, string, string) error {
		interruptStarted <- struct{}{}
		<-releaseInterrupt
		return nil
	}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: &fakeNotifier{}})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool {
		_, runs, err := st.GetTask(ctx, task.ID)
		return err == nil && len(runs) == 1 && runs[0].CodexTurnID == "turn-1"
	})
	returned := make(chan error, 1)
	go func() { returned <- controller.Stop(ctx, task.ID) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stop waited for the app-server interrupt response")
	}
	waitFor(t, interruptStarted)
	close(releaseInterrupt)
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "canceled"
	})
}

func TestControllerPersistsCompletionBeforeStartTurnResponse(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	releaseStartTurn := make(chan struct{})
	api.startTurn = func(context.Context, appserver.TurnStartInput) (appserver.Turn, error) {
		<-releaseStartTurn
		return appserver.Turn{ID: "turn-1", Status: "inProgress"}, nil
	}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: &fakeNotifier{}})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	api.events <- appserver.Event{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)}
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "succeeded"
	})
	close(releaseStartTurn)
	_, runs, err := st.GetTask(ctx, task.ID)
	if err != nil || len(runs) != 1 || runs[0].CodexTurnID != "turn-1" {
		t.Fatalf("completion before response lost turn id: runs=%+v err=%v", runs, err)
	}
}

func TestControllerStopsTurnStartedBeforeRPCResponseOnlyOnce(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	releaseStartTurn := make(chan struct{})
	api.startTurn = func(context.Context, appserver.TurnStartInput) (appserver.Turn, error) {
		<-releaseStartTurn
		return appserver.Turn{ID: "turn-1", Status: "inProgress"}, nil
	}
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: &fakeNotifier{}})
	defer controller.Close()
	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	if err := controller.Stop(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := controller.Stop(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	close(releaseStartTurn)
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "canceled"
	})
	if got := api.interrupts(); got != 1 {
		t.Fatalf("interrupt count = %d, want 1", got)
	}
}

func TestControllerStopsPendingTurnWhenStartTurnTimesOut(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	api.startTurn = func(ctx context.Context, _ appserver.TurnStartInput) (appserver.Turn, error) {
		<-ctx.Done()
		return appserver.Turn{}, ctx.Err()
	}
	controller := New(ControllerOptions{
		AppServer:        api,
		Store:            st,
		Notifier:         &fakeNotifier{},
		AppServerTimeout: 20 * time.Millisecond,
	})
	defer controller.Close()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "") != nil })
	if err := controller.Stop(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "canceled"
	})
}

func TestControllerDoesNotBlockEventLoopOnTerminalDelivery(t *testing.T) {
	ctx := context.Background()
	st, task, run := newQueuedTask(t, ctx)
	defer func() { _ = st.Close() }()
	api := newFakeAPI()
	notes := newBlockingTerminalNotifier()
	controller := New(ControllerOptions{AppServer: api, Store: st, Notifier: notes})
	defer controller.Close()
	defer notes.release()

	if err := controller.Enqueue(ctx, StartInput{Task: task, Run: run, Project: config.ResolvedProject{CWD: task.CWD}, CardMessageID: "card", DedupKey: "new"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, api.startedTurn)
	waitUntil(t, func() bool { return controller.activeFor("thread-1", "turn-1") != nil })

	returned := make(chan struct{})
	go func() {
		controller.handleEvent(appserver.Event{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("terminal event waited for card delivery")
	}
	waitFor(t, notes.started)
	notes.release()
	waitUntil(t, func() bool {
		stored, _, err := st.GetTask(ctx, task.ID)
		return err == nil && stored.Status == "succeeded"
	})
}

func TestAutomaticPermissionResponse(t *testing.T) {
	accepted, err := approvalResponse("item/permissions/requestApproval", json.RawMessage(`{"permissions":{"network":{"enabled":true}}}`), "approved")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(accepted)
	if string(data) != `{"permissions":{"network":{"enabled":true}},"scope":"turn"}` {
		t.Fatalf("unexpected permission response: %s", data)
	}
	declined, err := approvalResponse("item/permissions/requestApproval", nil, "declined")
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(declined)
	if string(data) != `{"permissions":{},"scope":"turn"}` {
		t.Fatalf("unexpected declined permissions response: %s", data)
	}
}

func TestAutoGrantMethodAllowlist(t *testing.T) {
	for _, method := range []string{
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
	} {
		if !isApprovalMethod(method) {
			t.Fatalf("known approval method %q was rejected", method)
		}
	}
	if isApprovalMethod("item/unknown/requestApproval") {
		t.Fatal("unknown approval method should not be sent to Feishu")
	}
}

func newQueuedTask(t *testing.T, ctx context.Context) (*store.Store, store.Task, store.Run) {
	t.Helper()
	st, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	admit, err := st.AdmitNewTask(ctx, "new", "message", store.CreateTaskInput{TaskID: "task", RunID: "run", CWD: "/repo", CreatedBy: "ou_owner", ChatID: "chat", Prompt: "work", Now: time.Now()})
	if err != nil {
		if closeErr := st.Close(); closeErr != nil {
			t.Fatalf("admit queued task: %v; close store: %v", err, closeErr)
		}
		t.Fatal(err)
	}
	return st, admit.Task, admit.Run
}

type fakeAPI struct {
	events      chan appserver.Event
	requests    chan appserver.ServerRequest
	startedTurn chan struct{}
	startTurn   func(context.Context, appserver.TurnStartInput) (appserver.Turn, error)
	steerTurn   func(context.Context, appserver.TurnSteerInput) (string, error)
	interruptFn func(context.Context, string, string) error

	mu          sync.Mutex
	responseLog []fakeResponse
	steerLog    []appserver.TurnSteerInput
	interrupt   int
}

type fakeResponse struct {
	methodID string
	decision string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{events: make(chan appserver.Event, 16), requests: make(chan appserver.ServerRequest, 16), startedTurn: make(chan struct{}, 4)}
}

func (f *fakeAPI) ListThreads(context.Context, int) ([]appserver.Thread, error) {
	return []appserver.Thread{{ID: "desktop-thread", CWD: "/repo"}}, nil
}
func (f *fakeAPI) StartThread(context.Context, appserver.ThreadStartInput) (appserver.Thread, error) {
	return appserver.Thread{ID: "thread-1", CWD: "/repo"}, nil
}
func (f *fakeAPI) ResumeThread(context.Context, appserver.ThreadResumeInput) (appserver.Thread, error) {
	return appserver.Thread{ID: "thread-1", CWD: "/repo"}, nil
}
func (f *fakeAPI) StartTurn(ctx context.Context, input appserver.TurnStartInput) (appserver.Turn, error) {
	f.startedTurn <- struct{}{}
	if f.startTurn != nil {
		return f.startTurn(ctx, input)
	}
	return appserver.Turn{ID: "turn-1", Status: "inProgress"}, nil
}
func (f *fakeAPI) SteerTurn(ctx context.Context, input appserver.TurnSteerInput) (string, error) {
	f.mu.Lock()
	f.steerLog = append(f.steerLog, input)
	f.mu.Unlock()
	if f.steerTurn != nil {
		return f.steerTurn(ctx, input)
	}
	return input.ExpectedTurnID, nil
}
func (f *fakeAPI) Interrupt(ctx context.Context, threadID, turnID string) error {
	f.mu.Lock()
	f.interrupt++
	f.mu.Unlock()
	if f.interruptFn != nil {
		return f.interruptFn(ctx, threadID, turnID)
	}
	return nil
}
func (f *fakeAPI) Respond(_ context.Context, id json.RawMessage, result any) error {
	data, _ := json.Marshal(result)
	var value struct {
		Decision string `json:"decision"`
	}
	_ = json.Unmarshal(data, &value)
	f.mu.Lock()
	f.responseLog = append(f.responseLog, fakeResponse{methodID: string(id), decision: value.Decision})
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) Events() <-chan appserver.Event           { return f.events }
func (f *fakeAPI) Requests() <-chan appserver.ServerRequest { return f.requests }
func (f *fakeAPI) Close() error                             { return nil }
func (f *fakeAPI) responses() []fakeResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeResponse(nil), f.responseLog...)
}
func (f *fakeAPI) steers() []appserver.TurnSteerInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appserver.TurnSteerInput(nil), f.steerLog...)
}
func (f *fakeAPI) interrupts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.interrupt }

type fakeNotifier struct {
	mu    sync.Mutex
	calls []string
}

type recordingNotifier struct {
	mu       sync.Mutex
	progress []notifier.TaskCardInput
}

func (n *recordingNotifier) Progress(_ context.Context, input notifier.TaskCardInput) (contracts.SentMessage, error) {
	n.mu.Lock()
	n.progress = append(n.progress, input)
	n.mu.Unlock()
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *recordingNotifier) Success(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *recordingNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *recordingNotifier) progressCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.progress)
}

func (n *recordingNotifier) hasProgress(match func(notifier.TaskCardInput) bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, input := range n.progress {
		if match(input) {
			return true
		}
	}
	return false
}

type transientProgressNotifier struct {
	mu      sync.Mutex
	needle  string
	failed  bool
	matched []notifier.TaskCardInput
}

func (n *transientProgressNotifier) Progress(_ context.Context, input notifier.TaskCardInput) (contracts.SentMessage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if strings.Contains(input.Presentation.ProcessingDetail, n.needle) {
		n.matched = append(n.matched, input)
		if !n.failed {
			n.failed = true
			return contracts.SentMessage{}, temporaryNotifierError{}
		}
	}
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *transientProgressNotifier) Success(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *transientProgressNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *transientProgressNotifier) matchCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.matched)
}

func (n *transientProgressNotifier) matchingUpdates() []notifier.TaskCardInput {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifier.TaskCardInput(nil), n.matched...)
}

type deadlineProgressNotifier struct {
	mu                   sync.Mutex
	progressCalls        int
	firstAttemptDuration time.Duration
	firstProgress        chan struct{}
	secondProgress       chan struct{}
}

func newDeadlineProgressNotifier() *deadlineProgressNotifier {
	return &deadlineProgressNotifier{
		firstProgress:  make(chan struct{}, 1),
		secondProgress: make(chan struct{}, 1),
	}
}

func (n *deadlineProgressNotifier) Progress(ctx context.Context, _ notifier.TaskCardInput) (contracts.SentMessage, error) {
	n.mu.Lock()
	n.progressCalls++
	attempt := n.progressCalls
	n.mu.Unlock()
	if attempt == 1 {
		select {
		case n.firstProgress <- struct{}{}:
		default:
		}
		started := time.Now()
		<-ctx.Done()
		n.mu.Lock()
		n.firstAttemptDuration = time.Since(started)
		n.mu.Unlock()
		return contracts.SentMessage{}, ctx.Err()
	}
	if attempt == 2 {
		select {
		case n.secondProgress <- struct{}{}:
		default:
		}
	}
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *deadlineProgressNotifier) Success(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *deadlineProgressNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *deadlineProgressNotifier) progressCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.progressCalls
}

func (n *deadlineProgressNotifier) firstAttemptElapsed() time.Duration {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.firstAttemptDuration
}

type terminalPatchNotifier struct {
	mu            sync.Mutex
	patchErr      error
	patchFailures int
	success       []notifier.TaskCardInput
}

func (n *terminalPatchNotifier) Progress(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *terminalPatchNotifier) Success(_ context.Context, input notifier.TaskCardInput) (contracts.SentMessage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.success = append(n.success, input)
	if input.UpdateMessageID != "" && n.patchFailures > 0 {
		n.patchFailures--
		return contracts.SentMessage{}, n.patchErr
	}
	return contracts.SentMessage{MessageID: "result-card"}, nil
}

func (n *terminalPatchNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *terminalPatchNotifier) successCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.success)
}

func (n *terminalPatchNotifier) successUpdates() []notifier.TaskCardInput {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifier.TaskCardInput(nil), n.success...)
}

type temporaryNotifierError struct{}

func (temporaryNotifierError) Error() string   { return "temporary timeout" }
func (temporaryNotifierError) Timeout() bool   { return true }
func (temporaryNotifierError) Temporary() bool { return true }

type blockingProgressNotifier struct {
	mu            sync.Mutex
	progress      []notifier.TaskCardInput
	firstProgress chan struct{}
	releaseFirst  chan struct{}
	releaseOnce   sync.Once
}

type blockingTerminalNotifier struct {
	started     chan struct{}
	releaseSend chan struct{}
	releaseOnce sync.Once
}

func newBlockingTerminalNotifier() *blockingTerminalNotifier {
	return &blockingTerminalNotifier{
		started:     make(chan struct{}, 1),
		releaseSend: make(chan struct{}),
	}
}

func (n *blockingTerminalNotifier) Progress(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *blockingTerminalNotifier) Success(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	select {
	case n.started <- struct{}{}:
	default:
	}
	<-n.releaseSend
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *blockingTerminalNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *blockingTerminalNotifier) release() {
	n.releaseOnce.Do(func() { close(n.releaseSend) })
}

func newBlockingProgressNotifier() *blockingProgressNotifier {
	return &blockingProgressNotifier{
		firstProgress: make(chan struct{}, 1),
		releaseFirst:  make(chan struct{}),
	}
}

func (n *blockingProgressNotifier) Progress(_ context.Context, input notifier.TaskCardInput) (contracts.SentMessage, error) {
	n.mu.Lock()
	n.progress = append(n.progress, input)
	first := len(n.progress) == 1
	n.mu.Unlock()
	if first {
		select {
		case n.firstProgress <- struct{}{}:
		default:
		}
		<-n.releaseFirst
	}
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *blockingProgressNotifier) Success(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *blockingProgressNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return contracts.SentMessage{MessageID: "card"}, nil
}

func (n *blockingProgressNotifier) release() {
	n.releaseOnce.Do(func() { close(n.releaseFirst) })
}

func (n *blockingProgressNotifier) progressCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.progress)
}

func (n *blockingProgressNotifier) progressUpdates() []notifier.TaskCardInput {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifier.TaskCardInput(nil), n.progress...)
}

func (f *fakeNotifier) Progress(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return f.record("progress"), nil
}
func (f *fakeNotifier) Success(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return f.record("success"), nil
}
func (f *fakeNotifier) Failure(context.Context, notifier.TaskCardInput) (contracts.SentMessage, error) {
	return f.record("failure"), nil
}
func (f *fakeNotifier) record(call string) contracts.SentMessage {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	return contracts.SentMessage{MessageID: "card"}
}
func (f *fakeNotifier) count(want string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == want {
			count++
		}
	}
	return count
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime")
	}
}

func waitUntil(t *testing.T, check func() bool) {
	waitUntilFor(t, 2*time.Second, check)
}

func waitUntilFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
