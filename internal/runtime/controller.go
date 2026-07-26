// Package runtime turns Feishu task requests into app-server turns and maps
// app-server notifications back to durable task state and card updates.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

var ErrNotRunning = errors.New("task has no running bridge turn")

const (
	progressUpdateInterval    = 400 * time.Millisecond
	progressRetryDelay        = 800 * time.Millisecond
	notificationTimeout       = 20 * time.Second
	terminalRetryAttempts     = 3
	defaultTerminalRetryDelay = time.Second
)

type TaskStore interface {
	StartRun(ctx context.Context, in store.StartRunInput) (store.Task, store.Run, error)
	FinishRun(ctx context.Context, dedupKey string, in store.FinishRunInput) error
	InsertMessageRoute(ctx context.Context, messageID, taskID, routeType string) error
}

type CardNotifier interface {
	Progress(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error)
	Success(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error)
	Failure(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error)
}

type StartInput struct {
	Task          store.Task
	Run           store.Run
	Project       config.ResolvedProject
	CardMessageID string
	DedupKey      string
}

type ControllerOptions struct {
	AppServer          appserver.API
	Store              TaskStore
	Notifier           CardNotifier
	Now                func() time.Time
	TerminalRetryDelay time.Duration
}

type Controller struct {
	api                appserver.API
	store              TaskStore
	notifier           CardNotifier
	now                func() time.Time
	terminalRetryDelay time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	lifecycleMu sync.Mutex
	closed      bool
	closeOnce   sync.Once

	mu       sync.Mutex
	byRun    map[string]*activeRun
	byTask   map[string]*activeRun
	byThread map[string]*activeRun
	byTurn   map[string]*activeRun
}

type activeRun struct {
	ctx    context.Context
	cancel context.CancelFunc

	project  config.ResolvedProject
	dedupKey string

	mu                  sync.Mutex
	task                store.Task
	run                 store.Run
	cardMessage         string
	threadID            string
	turnID              string
	finalText           string
	lastProgress        time.Time
	progressRetryAfter  time.Time
	pendingProgressBody string
	progressDirty       bool
	progressForce       bool
	progressWorker      bool
	progressWake        chan struct{}
	terminal            bool
	stopRequested       bool
	stopHandled         bool
	cardMu              sync.Mutex
}

func New(opts ControllerOptions) *Controller {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	retryDelay := opts.TerminalRetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultTerminalRetryDelay
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		api:                opts.AppServer,
		store:              opts.Store,
		notifier:           opts.Notifier,
		now:                now,
		terminalRetryDelay: retryDelay,
		ctx:                ctx,
		cancel:             cancel,
		byRun:              make(map[string]*activeRun),
		byTask:             make(map[string]*activeRun),
		byThread:           make(map[string]*activeRun),
		byTurn:             make(map[string]*activeRun),
	}
	if c.api != nil {
		c.start(c.eventLoop)
		c.start(c.requestLoop)
	}
	return c
}

func (c *Controller) Probe(ctx context.Context) error {
	if c.api == nil {
		return errors.New("app-server client is nil")
	}
	if _, err := c.api.ListThreads(ctx, 1); err != nil {
		return fmt.Errorf("discover desktop Codex threads: %w", err)
	}
	return nil
}

func (c *Controller) Threads(ctx context.Context, limit int) ([]appserver.Thread, error) {
	if c.api == nil {
		return nil, errors.New("app-server client is nil")
	}
	threads, err := c.api.ListThreads(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list desktop Codex threads: %w", err)
	}
	return threads, nil
}

func (c *Controller) Enqueue(ctx context.Context, input StartInput) error {
	if c.api == nil || c.store == nil || c.notifier == nil {
		return errors.New("runtime dependencies are not configured")
	}
	if input.Task.ID == "" || input.Run.ID == "" || input.CardMessageID == "" {
		return errors.New("task, run, and card message id are required")
	}
	if input.Run.Kind != "new" && input.Run.Kind != "resume" {
		return fmt.Errorf("unsupported run kind %q", input.Run.Kind)
	}
	runCtx, cancel := context.WithCancel(c.ctx)
	active := &activeRun{
		ctx:          runCtx,
		cancel:       cancel,
		project:      input.Project,
		dedupKey:     input.DedupKey,
		task:         input.Task,
		run:          input.Run,
		cardMessage:  input.CardMessageID,
		progressWake: make(chan struct{}, 1),
	}
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		cancel()
		return ErrNotRunning
	}
	c.mu.Lock()
	if _, exists := c.byTask[input.Task.ID]; exists {
		c.mu.Unlock()
		c.lifecycleMu.Unlock()
		cancel()
		return ErrNotRunning
	}
	c.byRun[input.Run.ID] = active
	c.byTask[input.Task.ID] = active
	c.mu.Unlock()
	c.wg.Add(1)
	c.lifecycleMu.Unlock()

	c.sendProgress(ctx, active, "已接收，正在连接 Codex。", true)
	go c.launch(active)
	return nil
}

func (c *Controller) Stop(ctx context.Context, taskID string) error {
	c.mu.Lock()
	active := c.byTask[taskID]
	c.mu.Unlock()
	if active == nil {
		return ErrNotRunning
	}
	active.requestStop()
	threadID, turnID := active.ids()
	if threadID == "" {
		active.cancel()
		if !c.start(func() { c.finish(active, "canceled", -1, "已停止。") }) {
			return ErrNotRunning
		}
		return nil
	}
	if turnID == "" {
		c.sendProgress(ctx, active, "正在停止 Codex。", true)
		return nil
	}
	if !active.takeStopRequest() {
		return nil
	}
	c.sendProgress(ctx, active, "正在停止 Codex。", true)
	if !c.start(func() {
		stopCtx, cancel := c.notificationContext()
		defer cancel()
		if err := c.api.Interrupt(stopCtx, threadID, turnID); err != nil {
			c.finish(active, "failed", -1, "停止 Codex 任务失败："+err.Error())
			return
		}
		active.cancel()
		c.finish(active, "canceled", -1, "已停止。")
	}) {
		return ErrNotRunning
	}
	return nil
}

func (c *Controller) Close() {
	c.closeOnce.Do(func() {
		c.lifecycleMu.Lock()
		c.closed = true
		c.cancel()
		c.lifecycleMu.Unlock()
		c.mu.Lock()
		for _, active := range c.byRun {
			active.cancel()
		}
		c.mu.Unlock()
		c.wg.Wait()
	})
}

func (c *Controller) launch(active *activeRun) {
	defer c.wg.Done()
	var (
		thread appserver.Thread
		err    error
	)
	if active.run.Kind == "new" {
		thread, err = c.api.StartThread(active.ctx, appserver.ThreadStartInput{
			CWD:   active.project.CWD,
			Model: active.project.Model,
		})
	} else {
		threadID, _ := active.ids()
		if threadID == "" {
			threadID = active.task.CodexThreadID
		}
		thread, err = c.api.ResumeThread(active.ctx, appserver.ThreadResumeInput{
			ThreadID: threadID,
			CWD:      active.task.CWD,
			Model:    active.project.Model,
		})
	}
	if err != nil {
		c.finishFromError(active, err)
		return
	}
	if thread.ID == "" {
		c.finish(active, "failed", -1, "Codex did not return a thread id.")
		return
	}
	if active.isTerminal() {
		return
	}
	c.setThread(active, thread.ID)
	if active.isTerminal() {
		return
	}
	task, run, err := c.store.StartRun(c.ctx, store.StartRunInput{RunID: active.runID(), ThreadID: thread.ID, Now: c.now()})
	if err != nil {
		c.finish(active, "failed", -1, "无法保存 Codex 运行状态："+err.Error())
		return
	}
	active.setState(task, run)
	if active.takeStopRequest() {
		c.finish(active, "canceled", -1, "已停止。")
		return
	}
	c.sendProgress(c.ctx, active, "Codex 正在处理。", true)

	turn, err := c.api.StartTurn(active.ctx, appserver.TurnStartInput{
		ThreadID: thread.ID,
		Text:     active.run.Prompt,
		CWD:      active.task.CWD,
		Model:    active.project.Model,
	})
	if err != nil {
		c.finishFromError(active, err)
		return
	}
	if turn.ID == "" {
		c.finish(active, "failed", -1, "Codex did not return a turn id.")
		return
	}
	c.setTurn(active, turn.ID)
	if active.isTerminal() {
		return
	}
	task, run, err = c.store.StartRun(c.ctx, store.StartRunInput{RunID: active.runID(), ThreadID: thread.ID, TurnID: turn.ID, Now: c.now()})
	if err != nil {
		c.finish(active, "failed", -1, "无法保存 Codex 运行状态："+err.Error())
		return
	}
	active.setState(task, run)
	if active.takeStopRequest() {
		stopCtx, cancel := c.notificationContext()
		err := c.api.Interrupt(stopCtx, thread.ID, turn.ID)
		cancel()
		if err != nil {
			c.finish(active, "failed", -1, "停止 Codex 任务失败："+err.Error())
			return
		}
		active.cancel()
		c.finish(active, "canceled", -1, "已停止。")
	}
}

func (c *Controller) eventLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case event, ok := <-c.api.Events():
			if !ok {
				return
			}
			c.handleEvent(event)
		}
	}
}

func (c *Controller) requestLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case request, ok := <-c.api.Requests():
			if !ok {
				return
			}
			c.handleRequest(request)
		}
	}
}

func (c *Controller) handleEvent(event appserver.Event) {
	switch event.Method {
	case "turn/started":
		var params turnStartedParams
		if json.Unmarshal(event.Params, &params) != nil {
			return
		}
		if active := c.activeFor(params.ThreadID, params.Turn.ID); active != nil {
			c.setTurn(active, params.Turn.ID)
		}
	case "item/agentMessage/delta":
		var params agentMessageDeltaParams
		if json.Unmarshal(event.Params, &params) != nil || params.Delta == "" {
			return
		}
		if active := c.activeFor(params.ThreadID, params.TurnID); active != nil {
			active.appendText(params.Delta)
			c.sendProgress(c.ctx, active, active.progressBody(), false)
		}
	case "item/completed":
		var params itemCompletedParams
		if json.Unmarshal(event.Params, &params) != nil || params.Item.Type != "agentMessage" {
			return
		}
		if active := c.activeFor(params.ThreadID, params.TurnID); active != nil {
			active.setFinalText(params.Item.Text)
		}
	case "turn/completed":
		var params turnCompletedParams
		if json.Unmarshal(event.Params, &params) != nil {
			return
		}
		active := c.activeFor(params.ThreadID, params.Turn.ID)
		if active == nil {
			return
		}
		// A terminal notification can arrive before turn/start returns. Retain the
		// notification's id so the durable result is still tied to that turn.
		c.setTurn(active, params.Turn.ID)
		status, exitCode := terminalStatus(params.Turn.Status)
		body := active.text()
		if body == "" && params.Turn.Error != nil {
			body = errorText(params.Turn.Error)
		}
		if body == "" && status == "failed" {
			body = "Codex turn failed without a final message."
		}
		if body == "" && status == "canceled" {
			body = "已停止。"
		}
		c.finish(active, status, exitCode, body)
	}
}

func (c *Controller) handleRequest(request appserver.ServerRequest) {
	if !isApprovalMethod(request.Method) {
		_ = c.api.Respond(c.ctx, request.ID, map[string]string{"decision": "decline"})
		return
	}
	response, err := approvalResponse(request.Method, request.Params, "approved")
	if err != nil {
		_ = c.api.Respond(c.ctx, request.ID, declineResponse(request.Method))
		return
	}
	_ = c.api.Respond(c.ctx, request.ID, response)
}

func (c *Controller) finishFromError(active *activeRun, err error) {
	if errors.Is(err, context.Canceled) {
		c.finish(active, "canceled", -1, "已停止。")
		return
	}
	c.finish(active, "failed", -1, err.Error())
}

func (c *Controller) finish(active *activeRun, status string, exitCode int, body string) {
	if !active.markTerminal() {
		return
	}
	active.cancel()
	c.unregister(active)
	threadID, turnID := active.ids()
	storeCtx, cancelStore := c.notificationContext()
	if err := c.store.FinishRun(storeCtx, active.dedupKey, store.FinishRunInput{
		RunID:      active.runID(),
		ThreadID:   threadID,
		TurnID:     turnID,
		Status:     status,
		ExitCode:   exitCode,
		FinalText:  body,
		FinishedAt: c.now(),
	}); err != nil {
		body = "无法保存 Codex 结果：" + err.Error()
		status = "failed"
	}
	cancelStore()
	input := notifier.TaskCardInput{
		ChatID:          active.chatID(),
		UpdateMessageID: active.cardID(),
		TaskID:          active.taskID(),
		Status:          status,
		ProjectAlias:    active.project.Alias,
		CWDLabel:        active.cwd(),
		Body:            body,
	}
	active.cardMu.Lock()
	notifyCtx, cancelNotify := c.notificationContext()
	sent, err := c.sendTerminal(notifyCtx, status, input)
	cancelNotify()
	active.cardMu.Unlock()
	if err != nil {
		if input.UpdateMessageID != "" && transport.IsTransientError(err) {
			slog.Warn("Feishu terminal card patch failed; retrying original card", "task_id", active.taskID(), "error", err)
			c.retryTerminalPatch(active, status, input)
			return
		}
		if input.UpdateMessageID != "" {
			c.sendTerminalFallback(active, status, input, err)
			return
		}
		slog.Error("Feishu terminal card delivery failed", "task_id", active.taskID(), "error", err)
		return
	}
	c.insertTerminalRoute(active, sent)
}

func (c *Controller) sendTerminal(ctx context.Context, status string, input notifier.TaskCardInput) (contracts.SentMessage, error) {
	if status == "succeeded" {
		return c.notifier.Success(ctx, input)
	}
	return c.notifier.Failure(ctx, input)
}

func (c *Controller) retryTerminalPatch(active *activeRun, status string, input notifier.TaskCardInput) {
	if !c.start(func() {
		var lastErr error
		for attempt := 1; attempt <= terminalRetryAttempts; attempt++ {
			if !c.waitForTerminalRetry(attempt) {
				return
			}
			active.cardMu.Lock()
			notifyCtx, cancel := c.notificationContext()
			sent, err := c.sendTerminal(notifyCtx, status, input)
			cancel()
			active.cardMu.Unlock()
			if err == nil {
				c.insertTerminalRoute(active, sent)
				return
			}
			if !transport.IsTransientError(err) {
				c.sendTerminalFallback(active, status, input, err)
				return
			}
			lastErr = err
			slog.Warn("Feishu terminal card patch retry failed", "task_id", active.taskID(), "attempt", attempt, "error", err)
		}
		slog.Error("Feishu terminal card could not be updated after retries", "task_id", active.taskID(), "error", lastErr)
		c.sendTerminalFallback(active, status, input, lastErr)
	}) {
		slog.Error("Feishu terminal card retry could not be scheduled", "task_id", active.taskID())
	}
}

func (c *Controller) waitForTerminalRetry(attempt int) bool {
	delay := c.terminalRetryDelay * time.Duration(attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *Controller) sendTerminalFallback(active *activeRun, status string, input notifier.TaskCardInput, patchErr error) {
	slog.Warn("Feishu terminal card cannot be patched; sending fallback result card", "task_id", active.taskID(), "error", patchErr)
	input.UpdateMessageID = ""
	active.cardMu.Lock()
	fallbackCtx, cancelFallback := c.notificationContext()
	sent, err := c.sendTerminal(fallbackCtx, status, input)
	cancelFallback()
	active.cardMu.Unlock()
	if err != nil {
		slog.Error("Feishu terminal fallback card delivery failed", "task_id", active.taskID(), "error", err)
		return
	}
	c.insertTerminalRoute(active, sent)
}

func (c *Controller) insertTerminalRoute(active *activeRun, sent contracts.SentMessage) {
	if sent.MessageID == "" || sent.MessageID == active.cardID() {
		return
	}
	routeCtx, cancelRoute := c.notificationContext()
	_ = c.store.InsertMessageRoute(routeCtx, sent.MessageID, active.taskID(), "result_card")
	cancelRoute()
}

func (c *Controller) sendProgress(_ context.Context, active *activeRun, body string, force bool) {
	if !active.queueProgress(body, force) {
		return
	}
	if !c.start(func() { c.flushProgress(active) }) {
		active.stopProgressWorker()
	}
}

// flushProgress keeps one card update in flight per task. While Feishu is
// processing that update, incoming deltas replace the pending body so the next
// patch always reflects the current stream rather than an outdated snapshot.
func (c *Controller) flushProgress(active *activeRun) {
	for {
		select {
		case <-active.ctx.Done():
			active.stopProgressWorker()
			return
		default:
		}

		body, wait, ok := active.nextProgress(c.now())
		if !ok {
			return
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-active.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				active.stopProgressWorker()
				return
			case <-active.progressWake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
			continue
		}

		var notifyErr error
		active.cardMu.Lock()
		if !active.isTerminal() {
			notifyCtx, cancel := c.notificationContext()
			_, notifyErr = c.notifier.Progress(notifyCtx, notifier.TaskCardInput{
				ChatID:          active.chatID(),
				UpdateMessageID: active.cardID(),
				TaskID:          active.taskID(),
				Status:          "running",
				ProjectAlias:    active.project.Alias,
				CWDLabel:        active.cwd(),
				Body:            body,
			})
			cancel()
		}
		active.cardMu.Unlock()
		if notifyErr != nil {
			transient := transport.IsTransientError(notifyErr)
			slog.Warn("Feishu progress card patch failed", "task_id", active.taskID(), "transient", transient, "error", notifyErr)
			if transient {
				active.retryProgress(body, c.now().Add(progressRetryDelay))
			}
		}
	}
}

func (c *Controller) notificationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), notificationTimeout)
}

func (c *Controller) setThread(active *activeRun, threadID string) {
	if threadID == "" {
		return
	}
	active.mu.Lock()
	if active.terminal {
		active.mu.Unlock()
		return
	}
	active.threadID = threadID
	active.mu.Unlock()
	c.mu.Lock()
	if !active.isTerminal() {
		c.byThread[threadID] = active
	}
	c.mu.Unlock()
}

func (c *Controller) setTurn(active *activeRun, turnID string) {
	if turnID == "" {
		return
	}
	threadID, _ := active.ids()
	active.mu.Lock()
	if active.terminal {
		active.mu.Unlock()
		return
	}
	active.turnID = turnID
	active.mu.Unlock()
	if threadID != "" {
		c.mu.Lock()
		if !active.isTerminal() {
			c.byTurn[turnKey(threadID, turnID)] = active
		}
		c.mu.Unlock()
	}
}

func (c *Controller) activeFor(threadID, turnID string) *activeRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	if turnID != "" {
		if active := c.byTurn[turnKey(threadID, turnID)]; active != nil {
			return active
		}
	}
	return c.byThread[threadID]
}

func (c *Controller) unregister(active *activeRun) {
	c.mu.Lock()
	delete(c.byRun, active.runID())
	delete(c.byTask, active.taskID())
	threadID, turnID := active.ids()
	if threadID != "" && c.byThread[threadID] == active {
		delete(c.byThread, threadID)
	}
	if turnID != "" && c.byTurn[turnKey(threadID, turnID)] == active {
		delete(c.byTurn, turnKey(threadID, turnID))
	}
	c.mu.Unlock()
}

func (c *Controller) start(fn func()) bool {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closed {
		return false
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		fn()
	}()
	return true
}

func (a *activeRun) ids() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	threadID := a.threadID
	if threadID == "" {
		threadID = a.task.CodexThreadID
	}
	return threadID, a.turnID
}

func (a *activeRun) taskID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.task.ID
}

func (a *activeRun) runID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.run.ID
}

func (a *activeRun) chatID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.task.ChatID
}

func (a *activeRun) cwd() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.task.CWD
}

func (a *activeRun) cardID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cardMessage
}

func (a *activeRun) setState(task store.Task, run store.Run) {
	a.mu.Lock()
	a.task = task
	a.run = run
	a.mu.Unlock()
}

func (a *activeRun) requestStop() {
	a.mu.Lock()
	a.stopRequested = true
	a.mu.Unlock()
}

func (a *activeRun) takeStopRequest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.stopRequested || a.stopHandled {
		return false
	}
	a.stopHandled = true
	return true
}

func (a *activeRun) appendText(delta string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finalText = trimStreamingText(a.finalText + delta)
}

func (a *activeRun) setFinalText(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if text != "" {
		a.finalText = text
	}
}

func (a *activeRun) text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.finalText)
}

func (a *activeRun) progressBody() string {
	text := a.text()
	if text == "" {
		return "Codex 正在处理。"
	}
	return "Codex 正在处理。\n\n" + trimStreamingText(text)
}

func (a *activeRun) queueProgress(body string, force bool) bool {
	if body == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal {
		return false
	}
	a.pendingProgressBody = body
	a.progressDirty = true
	a.progressForce = a.progressForce || force
	if a.progressWake != nil {
		select {
		case a.progressWake <- struct{}{}:
		default:
		}
	}
	if a.progressWorker {
		return false
	}
	a.progressWorker = true
	return true
}

func (a *activeRun) nextProgress(now time.Time) (body string, wait time.Duration, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal || !a.progressDirty {
		a.progressWorker = false
		return "", 0, false
	}
	if !a.progressForce && !a.progressRetryAfter.IsZero() {
		if remaining := a.progressRetryAfter.Sub(now); remaining > 0 {
			return "", remaining, true
		}
		a.progressRetryAfter = time.Time{}
	}
	if !a.progressForce && !a.lastProgress.IsZero() {
		if remaining := progressUpdateInterval - now.Sub(a.lastProgress); remaining > 0 {
			return "", remaining, true
		}
	}
	body = a.pendingProgressBody
	a.progressDirty = false
	a.progressForce = false
	a.lastProgress = now
	return body, 0, true
}

func (a *activeRun) retryProgress(body string, retryAfter time.Time) {
	if body == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal {
		return
	}
	if !a.progressDirty {
		a.pendingProgressBody = body
		a.progressDirty = true
	}
	a.progressRetryAfter = retryAfter
}

func (a *activeRun) stopProgressWorker() {
	a.mu.Lock()
	a.progressWorker = false
	a.mu.Unlock()
}

func (a *activeRun) markTerminal() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminal {
		return false
	}
	a.terminal = true
	return true
}

func (a *activeRun) isTerminal() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.terminal
}

type turnStartedParams struct {
	ThreadID string         `json:"threadId"`
	Turn     appserver.Turn `json:"turn"`
}

type agentMessageDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Delta    string `json:"delta"`
}

type itemCompletedParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

type turnCompletedParams struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	} `json:"turn"`
}

func terminalStatus(value string) (string, int) {
	switch value {
	case "completed":
		return "succeeded", 0
	case "interrupted":
		return "canceled", -1
	default:
		return "failed", -1
	}
}

func isApprovalMethod(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		return true
	default:
		return false
	}
}

func approvalResponse(method string, params json.RawMessage, decision string) (any, error) {
	if decision != "approved" && decision != "declined" {
		return nil, fmt.Errorf("invalid approval decision %q", decision)
	}
	if method == "item/permissions/requestApproval" {
		if decision == "declined" {
			return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
		}
		var request struct {
			Permissions json.RawMessage `json:"permissions"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, err
		}
		var permissions any = map[string]any{}
		if len(request.Permissions) > 0 && string(request.Permissions) != "null" {
			if err := json.Unmarshal(request.Permissions, &permissions); err != nil {
				return nil, err
			}
		}
		return map[string]any{"permissions": permissions, "scope": "turn"}, nil
	}
	response := "decline"
	if decision == "approved" {
		response = "accept"
	}
	return map[string]string{"decision": response}, nil
}

func declineResponse(method string) any {
	response, err := approvalResponse(method, nil, "declined")
	if err != nil {
		return map[string]string{"decision": "decline"}
	}
	return response
}

func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var message struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &message) == nil && message.Message != "" {
		return message.Message
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func trimStreamingText(text string) string {
	const limit = 3200
	if len(text) <= limit {
		return text
	}
	return "..." + text[len(text)-limit:]
}

func turnKey(threadID, turnID string) string {
	return threadID + "\x00" + turnID
}
