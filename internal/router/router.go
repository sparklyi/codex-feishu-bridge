package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/appserver"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/intent"
	"github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/runtime"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

var errActionCreatorMismatch = errors.New("task creator mismatch")

type TaskStore interface {
	AdmitNewTask(ctx context.Context, dedupKey, source string, in store.CreateTaskInput) (store.AdmitResult, error)
	AttachThread(ctx context.Context, dedupKey, source string, in store.AttachThreadInput) (store.Task, bool, error)
	AdmitRestart(ctx context.Context, dedupKey, source string, now time.Time) (bool, error)
	AdmitResumeRun(ctx context.Context, dedupKey, source string, in store.ResumeRunInput) (store.AdmitResult, error)
	FinishRun(ctx context.Context, dedupKey string, in store.FinishRunInput) error
	UserEnabled(ctx context.Context, openID string) (bool, error)
	HasActiveTask(ctx context.Context) (bool, error)
	InsertMessageRoute(ctx context.Context, messageID, taskID, routeType string) error
	SetTaskRootMessageID(ctx context.Context, taskID, messageID string, now time.Time) error
	ResolveMessageRoute(ctx context.Context, messageID string) (store.Task, error)
	GetTask(ctx context.Context, taskID string) (store.Task, []store.Run, error)
}

type Controller interface {
	Threads(ctx context.Context, limit int) ([]appserver.Thread, error)
	Enqueue(ctx context.Context, input runtime.StartInput) error
	Steer(ctx context.Context, taskID, text string) error
	Stop(ctx context.Context, taskID string) error
}

type Notifier interface {
	Start(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error)
	Failure(ctx context.Context, in notifier.TaskCardInput) (contracts.SentMessage, error)
	ThreadSelection(ctx context.Context, in notifier.ThreadSelectionInput) (contracts.SentMessage, error)
	RoutingError(ctx context.Context, chatID, replyToMessageID string) (contracts.SentMessage, error)
	Rejection(ctx context.Context, chatID, replyToMessageID, body string) error
	RunningConflict(ctx context.Context, in notifier.RunningConflictInput) error
	Restarting(ctx context.Context, chatID, replyToMessageID string) error
}

type RouterOptions struct {
	Config     config.Config
	Store      TaskStore
	Controller Controller
	Notifier   Notifier
	Now        func() time.Time
	NewTaskID  func() string
	NewRunID   func() string
	Restart    func()
}

type Router struct {
	cfg        config.Config
	store      TaskStore
	controller Controller
	notifier   Notifier
	now        func() time.Time
	newTaskID  func() string
	newRunID   func() string
	restart    func()
}

func New(opts RouterOptions) *Router {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newTaskID := opts.NewTaskID
	if newTaskID == nil {
		newTaskID = func() string { return randomID("task") }
	}
	newRunID := opts.NewRunID
	if newRunID == nil {
		newRunID = func() string { return randomID("run") }
	}
	return &Router{
		cfg:        opts.Config,
		store:      opts.Store,
		controller: opts.Controller,
		notifier:   opts.Notifier,
		now:        now,
		newTaskID:  newTaskID,
		newRunID:   newRunID,
		restart:    opts.Restart,
	}
}

func (r *Router) Handle(ctx context.Context, ev contracts.InboundEvent) error {
	if ev.ChatType != "private" {
		return nil
	}
	authorized, err := r.authorized(ctx, ev.SenderOpenID)
	if err != nil {
		return fmt.Errorf("authorize Feishu user: %w", err)
	}
	if !authorized {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "当前用户未获授权使用 Codex Bridge。")
	}
	if intent.IsRestartService(ev.Text) {
		return r.handleRestart(ctx, ev)
	}
	switch ev.Kind {
	case contracts.InboundNewTask:
		return r.handleNewTask(ctx, ev)
	case contracts.InboundCardAction:
		action := ev.ActionValue["action"]
		if action == "" {
			action = ev.ActionID
		}
		switch action {
		case "attach_thread":
			return r.handleAttachThread(ctx, ev)
		case "stop_task":
			return r.handleStop(ctx, ev)
		case "steer":
			return r.handleSteer(ctx, ev)
		case "continue":
			return r.handleContinuation(ctx, ev)
		}
		if action != "" {
			return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "该卡片操作已不再支持。")
		}
		return r.handleContinuation(ctx, ev)
	case contracts.InboundReply:
		return r.handleContinuation(ctx, ev)
	default:
		return nil
	}
}

func (r *Router) handleNewTask(ctx context.Context, ev contracts.InboundEvent) error {
	parsed := intent.ParseStart(intent.ParseInput{Text: ev.Text, ProjectAliases: r.cfg.ProjectAliases()})
	switch parsed.Kind {
	case intent.KindIgnored:
		return nil
	case intent.KindThreadSelection:
		return r.sendThreadSelection(ctx, ev)
	case intent.KindRestartService:
		return r.handleRestart(ctx, ev)
	case intent.KindUnknownProject:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "项目配置错误：未找到项目 "+parsed.ProjectAlias)
	case intent.KindStartTask:
		return r.startTask(ctx, ev, parsed.ProjectAlias, parsed.Prompt)
	default:
		return nil
	}
}

func (r *Router) handleRestart(ctx context.Context, ev contracts.InboundEvent) error {
	if r.restart == nil {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "当前服务未由受监督进程启动，无法安全重启。")
	}
	active, err := r.store.HasActiveTask(ctx)
	if err != nil {
		return fmt.Errorf("check active tasks before restart: %w", err)
	}
	if active {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "当前仍有 Codex 任务运行。请等待完成或先停止任务后再重启服务。")
	}
	admitted, err := r.store.AdmitRestart(ctx, ev.DedupKey, sourceFor(ev), r.now())
	if err != nil {
		return fmt.Errorf("admit service restart: %w", err)
	}
	if !admitted {
		return nil
	}
	if err := r.notifier.Restarting(ctx, ev.ChatID, ev.MessageID); err != nil {
		return err
	}
	r.restart()
	return nil
}

func (r *Router) sendThreadSelection(ctx context.Context, ev contracts.InboundEvent) error {
	if r.controller == nil {
		return errors.New("runtime controller is not configured")
	}
	threads, err := r.controller.Threads(ctx, r.cfg.Runtime.ThreadSelectionLimitValue())
	if err != nil {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "无法发现桌面 Codex 会话："+err.Error())
	}
	options := make([]notifier.ThreadOption, 0, len(threads))
	for _, thread := range threads {
		options = append(options, notifier.ThreadOption{
			ID:      thread.ID,
			Name:    thread.Name,
			Preview: thread.Preview,
			CWD:     thread.CWD,
			Status:  thread.Status.Type,
		})
	}
	_, err = r.notifier.ThreadSelection(ctx, notifier.ThreadSelectionInput{ChatID: ev.ChatID, ReplyToMessageID: ev.MessageID, Threads: options})
	return err
}

func (r *Router) handleAttachThread(ctx context.Context, ev contracts.InboundEvent) error {
	threadID := ev.ActionValue["thread_id"]
	if threadID == "" {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "会话标识缺失。")
	}
	threads, err := r.controller.Threads(ctx, r.cfg.Runtime.ThreadLookupLimitValue())
	if err != nil {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "无法验证桌面 Codex 会话："+err.Error())
	}
	thread, ok := findThread(threads, threadID)
	if !ok {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "该 Codex 会话已不存在，请重新发送 /sessions。")
	}
	if thread.CWD == "" {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "该 Codex 会话没有可用工作目录。")
	}
	alias := r.cfg.ProjectAliasForCWD(thread.CWD)
	task, replay, err := r.store.AttachThread(ctx, ev.DedupKey, sourceFor(ev), store.AttachThreadInput{
		TaskID:       r.newTaskID(),
		ThreadID:     thread.ID,
		ProjectAlias: alias,
		CWD:          thread.CWD,
		CreatedBy:    ev.SenderOpenID,
		ChatID:       ev.ChatID,
		Now:          r.now(),
	})
	if err != nil || replay {
		return err
	}
	sent, err := r.notifier.Start(ctx, notifier.TaskCardInput{
		ChatID:       task.ChatID,
		TaskID:       task.ID,
		Status:       "idle",
		ProjectAlias: task.ProjectAlias,
		CWDLabel:     task.CWD,
		Body:         "桌面 Codex 会话已接管。",
	})
	if err != nil {
		return err
	}
	if err := r.store.SetTaskRootMessageID(ctx, task.ID, sent.MessageID, r.now()); err != nil {
		return err
	}
	return r.insertRouteWithRetry(ctx, sent.MessageID, task.ID, "start_card")
}

func (r *Router) startTask(ctx context.Context, ev contracts.InboundEvent, alias, prompt string) error {
	project, err := r.cfg.ResolveProject(alias)
	if err != nil {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "项目配置错误："+err.Error())
	}
	admit, err := r.store.AdmitNewTask(ctx, ev.DedupKey, sourceFor(ev), store.CreateTaskInput{
		TaskID:       r.newTaskID(),
		RunID:        r.newRunID(),
		ProjectAlias: alias,
		CWD:          project.CWD,
		CreatedBy:    ev.SenderOpenID,
		ChatID:       ev.ChatID,
		Prompt:       prompt,
		Now:          r.now(),
	})
	if err != nil || admit.Replay {
		return err
	}
	sent, err := r.notifier.Start(ctx, notifier.TaskCardInput{
		ChatID:       admit.Task.ChatID,
		TaskID:       admit.Task.ID,
		Status:       "queued",
		ProjectAlias: admit.Task.ProjectAlias,
		CWDLabel:     admit.Task.CWD,
		Body:         "正在创建 Codex 会话。",
		UserInputs:   []string{admit.Run.Prompt},
	})
	if err != nil {
		return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
	}
	if err := r.store.SetTaskRootMessageID(ctx, admit.Task.ID, sent.MessageID, r.now()); err != nil {
		return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
	}
	if err := r.insertRouteWithRetry(ctx, sent.MessageID, admit.Task.ID, "start_card"); err != nil {
		return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
	}
	if err := r.controller.Enqueue(ctx, runtime.StartInput{Task: admit.Task, Run: admit.Run, Project: project, CardMessageID: sent.MessageID, DedupKey: ev.DedupKey}); err != nil {
		finishErr := r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
		_, notifyErr := r.notifier.Failure(ctx, taskCard(admit.Task, "failed", err.Error(), sent.MessageID, admit.Run.Prompt))
		return errors.Join(finishErr, notifyErr)
	}
	return nil
}

func (r *Router) handleContinuation(ctx context.Context, ev contracts.InboundEvent) error {
	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "继续跟进需要填写内容。")
	}
	return r.resumeTask(ctx, ev, text)
}

func (r *Router) resumeTask(ctx context.Context, ev contracts.InboundEvent, text string) error {
	task, err := r.resolveContinuationTask(ctx, ev)
	if errors.Is(err, store.ErrRouteMiss) || errors.Is(err, store.ErrNotFound) {
		_, sendErr := r.notifier.RoutingError(ctx, ev.ChatID, ev.MessageID)
		return sendErr
	}
	if err != nil {
		return err
	}
	admit, err := r.store.AdmitResumeRun(ctx, ev.DedupKey, sourceFor(ev), store.ResumeRunInput{RunID: r.newRunID(), TaskID: task.ID, RequestedBy: ev.SenderOpenID, Prompt: text, Now: r.now()})
	if err != nil || admit.Replay {
		return err
	}
	switch admit.Reason {
	case store.RejectNone:
	case store.RejectCreatorMismatch:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "只有任务创建者可以继续此任务。")
	case store.RejectActiveRun:
		return r.notifier.RunningConflict(ctx, notifier.RunningConflictInput{ChatID: ev.ChatID, ReplyToMessageID: ev.MessageID, TaskID: task.ID, Status: task.Status, ProjectAlias: task.ProjectAlias})
	case store.RejectMissingThread:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "此任务没有可继续的 Codex 会话。")
	default:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "此任务当前不能继续。")
	}
	project, err := r.cfg.ResolveProject(admit.Task.ProjectAlias)
	if err != nil {
		finishErr := r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
		return errors.Join(finishErr, r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "项目配置错误："+err.Error()))
	}
	project.CWD = admit.Task.CWD
	cardID := admit.Task.RootMessageID
	if cardID == "" {
		sent, sendErr := r.notifier.Start(ctx, taskCard(admit.Task, "queued", "正在恢复 Codex 会话。", "", admit.Run.Prompt))
		if sendErr != nil {
			return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, sendErr)
		}
		cardID = sent.MessageID
		if err := r.store.SetTaskRootMessageID(ctx, admit.Task.ID, cardID, r.now()); err != nil {
			return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
		}
		if err := r.insertRouteWithRetry(ctx, cardID, admit.Task.ID, "start_card"); err != nil {
			return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
		}
	}
	if err := r.controller.Enqueue(ctx, runtime.StartInput{Task: admit.Task, Run: admit.Run, Project: project, CardMessageID: cardID, DedupKey: ev.DedupKey}); err != nil {
		return r.failQueuedRun(admit.Task, admit.Run, ev.DedupKey, err)
	}
	return nil
}

func (r *Router) handleStop(ctx context.Context, ev contracts.InboundEvent) error {
	task, _, err := r.store.GetTask(ctx, ev.ActionValue["task_id"])
	if errors.Is(err, store.ErrNotFound) {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "任务不存在。")
	}
	if err != nil {
		return err
	}
	if task.ChatID != ev.ChatID || task.CreatedBy != ev.SenderOpenID {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "无权停止此任务。")
	}
	if err := r.controller.Stop(ctx, task.ID); err != nil {
		if errors.Is(err, runtime.ErrNotRunning) {
			return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "此任务当前没有可停止的桥接操作。")
		}
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "停止任务失败："+err.Error())
	}
	return nil
}

func (r *Router) handleSteer(ctx context.Context, ev contracts.InboundEvent) error {
	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "补充到本轮需要填写内容。")
	}
	task, _, err := r.actionTask(ctx, ev)
	if err != nil {
		return r.actionTaskError(ctx, ev, err)
	}
	if err := r.controller.Steer(ctx, task.ID, text); err != nil {
		if errors.Is(err, runtime.ErrNotRunning) {
			return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "当前任务已结束，请使用继续跟进发起下一轮。")
		}
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "补充当前任务失败："+err.Error())
	}
	return nil
}

func (r *Router) actionTask(ctx context.Context, ev contracts.InboundEvent) (store.Task, []store.Run, error) {
	taskID := ev.ActionValue["task_id"]
	if taskID == "" {
		return store.Task{}, nil, errors.New("missing task id")
	}
	task, runs, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		return store.Task{}, nil, err
	}
	if task.ChatID != ev.ChatID {
		return store.Task{}, nil, store.ErrRouteMiss
	}
	if task.CreatedBy != ev.SenderOpenID {
		return store.Task{}, nil, errActionCreatorMismatch
	}
	return task, runs, nil
}

func (r *Router) actionTaskError(ctx context.Context, ev contracts.InboundEvent, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "任务不存在。")
	case errors.Is(err, store.ErrRouteMiss):
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "无权操作此任务。")
	case errors.Is(err, errActionCreatorMismatch):
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "只有任务创建者可以操作此任务。")
	default:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "任务操作失败："+err.Error())
	}
}

func (r *Router) resolveContinuationTask(ctx context.Context, ev contracts.InboundEvent) (store.Task, error) {
	task, err := r.store.ResolveMessageRoute(ctx, ev.RootMessageID)
	if !errors.Is(err, store.ErrRouteMiss) {
		return task, err
	}
	if ev.Kind != contracts.InboundCardAction {
		return store.Task{}, err
	}
	taskID := ev.ActionValue["task_id"]
	if taskID == "" {
		return store.Task{}, err
	}
	task, _, getErr := r.store.GetTask(ctx, taskID)
	if getErr != nil {
		return store.Task{}, getErr
	}
	if task.ChatID != ev.ChatID {
		return store.Task{}, store.ErrRouteMiss
	}
	return task, nil
}

func (r *Router) failQueuedRun(task store.Task, run store.Run, dedupKey string, cause error) error {
	if cause == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.Runtime.NotificationTimeout())
	defer cancel()
	if err := r.store.FinishRun(ctx, dedupKey, store.FinishRunInput{
		RunID:      run.ID,
		ThreadID:   task.CodexThreadID,
		Status:     "failed",
		ExitCode:   -1,
		FinalText:  cause.Error(),
		FinishedAt: r.now(),
	}); err != nil {
		return errors.Join(cause, fmt.Errorf("mark queued run failed: %w", err))
	}
	return cause
}

func (r *Router) insertRouteWithRetry(ctx context.Context, messageID, taskID, routeType string) error {
	var err error
	attempts := r.cfg.Runtime.RouteInsertAttemptsValue()
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		err = r.store.InsertMessageRoute(ctx, messageID, taskID, routeType)
		if err == nil {
			return nil
		}
	}
	return err
}

func (r *Router) authorized(ctx context.Context, openID string) (bool, error) {
	if openID == "" {
		return false, nil
	}
	return r.store.UserEnabled(ctx, openID)
}

func taskCard(task store.Task, status, body, updateMessageID string, userInputs ...string) notifier.TaskCardInput {
	return notifier.TaskCardInput{
		ChatID:          task.ChatID,
		UpdateMessageID: updateMessageID,
		TaskID:          task.ID,
		Status:          status,
		ProjectAlias:    task.ProjectAlias,
		CWDLabel:        task.CWD,
		Body:            body,
		UserInputs:      userInputs,
	}
}

func sourceFor(ev contracts.InboundEvent) string {
	if ev.Kind == contracts.InboundCardAction {
		return "card_callback"
	}
	return "message"
}

func findThread(threads []appserver.Thread, threadID string) (appserver.Thread, bool) {
	for _, thread := range threads {
		if thread.ID == threadID {
			return thread, true
		}
	}
	return appserver.Thread{}, false
}

func randomID(prefix string) string {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}
