package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/codexrunner"
	"github.com/sparklyi/codex-feishu-bridge/internal/config"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/intent"
	notify "github.com/sparklyi/codex-feishu-bridge/internal/notifier"
	"github.com/sparklyi/codex-feishu-bridge/internal/store"
)

type TaskStore interface {
	AdmitNewTask(ctx context.Context, dedupKey, source string, in store.CreateTaskInput) (store.AdmitResult, error)
	AdmitResumeRun(ctx context.Context, dedupKey, source string, in store.ResumeRunInput) (store.AdmitResult, error)
	RecordRunSession(ctx context.Context, runID, threadID string) error
	FinishRun(ctx context.Context, dedupKey, runID string, result contracts.RunResult, status string) error
	FailDedup(ctx context.Context, dedupKey string, err error) error
	UserEnabled(ctx context.Context, openID string) (bool, error)
	InsertMessageRoute(ctx context.Context, messageID, taskID, routeType string) error
	ResolveMessageRoute(ctx context.Context, messageID string) (store.Task, error)
	CreatePendingIntent(ctx context.Context, in store.CreatePendingIntentInput) (store.PendingIntent, error)
	ConsumePendingIntent(ctx context.Context, id, createdBy string, now time.Time) (store.PendingIntent, error)
	FindRunningTask(ctx context.Context, chatID, creatorOpenID string) (store.Task, bool, error)
}

type Runner interface {
	Exec(ctx context.Context, in codexrunner.ExecInput) (contracts.RunResult, error)
	Resume(ctx context.Context, in codexrunner.ResumeInput) (contracts.RunResult, error)
}

type Notifier interface {
	Start(ctx context.Context, in notify.TaskCardInput) (contracts.SentMessage, error)
	Success(ctx context.Context, in notify.TaskCardInput) (contracts.SentMessage, error)
	Failure(ctx context.Context, in notify.TaskCardInput) (contracts.SentMessage, error)
	RoutingError(ctx context.Context, chatID, replyToMessageID string) (contracts.SentMessage, error)
	Rejection(ctx context.Context, chatID, replyToMessageID, body string) error
	MigrationHint(ctx context.Context, chatID, replyToMessageID string) error
	ProjectSelection(ctx context.Context, in notify.ProjectSelectionInput) (contracts.SentMessage, error)
	RunningConflict(ctx context.Context, in notify.RunningConflictInput) error
}

type RouterOptions struct {
	Config       config.Config
	Store        TaskStore
	Runner       Runner
	Notifier     Notifier
	Now          func() time.Time
	NewTaskID    func() string
	NewRunID     func() string
	NewPendingID func() string
}

type Router struct {
	cfg          config.Config
	store        TaskStore
	runner       Runner
	notifier     Notifier
	now          func() time.Time
	newTaskID    func() string
	newRunID     func() string
	newPendingID func() string
}

func New(opts RouterOptions) *Router {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newTaskID := opts.NewTaskID
	if newTaskID == nil {
		newTaskID = randomTaskID
	}
	newRunID := opts.NewRunID
	if newRunID == nil {
		newRunID = func() string { return "run_" + time.Now().UTC().Format("20060102150405.000000000") }
	}
	newPendingID := opts.NewPendingID
	if newPendingID == nil {
		newPendingID = randomPendingID
	}
	return &Router{cfg: opts.Config, store: opts.Store, runner: opts.Runner, notifier: opts.Notifier, now: now, newTaskID: newTaskID, newRunID: newRunID, newPendingID: newPendingID}
}

func (r *Router) Handle(ctx context.Context, ev contracts.InboundEvent) error {
	if !r.authorized(ctx, ev.SenderOpenID) {
		if ev.ChatType == "private" {
			return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "You are not authorized to run Codex from this bridge.")
		}
		return nil
	}
	switch ev.Kind {
	case contracts.InboundNewTask:
		return r.handleNewTask(ctx, ev)
	case contracts.InboundCardAction:
		if ev.ActionValue["action"] == "select_project" {
			return r.handleProjectSelection(ctx, ev)
		}
		return r.handleContinuation(ctx, ev)
	case contracts.InboundReply:
		return r.handleContinuation(ctx, ev)
	default:
		return nil
	}
}

func (r *Router) handleNewTask(ctx context.Context, ev contracts.InboundEvent) error {
	parsed := intent.ParseStart(intent.ParseInput{Event: ev, ProjectAliases: r.cfg.ProjectAliases()})
	switch parsed.Kind {
	case intent.KindIgnored:
		return nil
	case intent.KindMigrationHint:
		return r.notifier.MigrationHint(ctx, ev.ChatID, ev.MessageID)
	case intent.KindUnknownProject:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "Project configuration error: unknown project alias "+parsed.ProjectAlias)
	case intent.KindProjectSelection:
		return r.sendProjectSelection(ctx, ev, parsed.Prompt)
	case intent.KindStartTask:
	default:
		return nil
	}
	return r.startTask(ctx, ev, parsed.ProjectAlias, parsed.Prompt)
}

func (r *Router) sendProjectSelection(ctx context.Context, ev contracts.InboundEvent, prompt string) error {
	pendingID := r.newPendingID()
	aliases := r.cfg.ProjectAliases()
	pending, err := r.store.CreatePendingIntent(ctx, store.CreatePendingIntentInput{
		ID:             pendingID,
		ChatID:         ev.ChatID,
		CreatedBy:      ev.SenderOpenID,
		Prompt:         prompt,
		ProjectAliases: aliases,
		Now:            r.now(),
		ExpiresAt:      r.now().Add(10 * time.Minute),
	})
	if err != nil {
		return err
	}
	_, err = r.notifier.ProjectSelection(ctx, notify.ProjectSelectionInput{
		ChatID:           ev.ChatID,
		ReplyToMessageID: ev.MessageID,
		PendingID:        pending.ID,
		Prompt:           pending.Prompt,
		ProjectAliases:   pending.ProjectAliases,
	})
	return err
}

func (r *Router) handleProjectSelection(ctx context.Context, ev contracts.InboundEvent) error {
	pending, err := r.store.ConsumePendingIntent(ctx, ev.ActionValue["pending_id"], ev.SenderOpenID, r.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	alias := ev.ActionValue["project"]
	if !containsProjectAlias(pending.ProjectAliases, alias) {
		_, sendErr := r.notifier.ProjectSelection(ctx, notify.ProjectSelectionInput{
			ChatID:           ev.ChatID,
			ReplyToMessageID: ev.MessageID,
			PendingID:        pending.ID,
			Prompt:           pending.Prompt,
			ProjectAliases:   pending.ProjectAliases,
		})
		return sendErr
	}
	return r.startTask(ctx, ev, alias, pending.Prompt)
}

func (r *Router) startTask(ctx context.Context, ev contracts.InboundEvent, alias, prompt string) error {
	running, ok, err := r.store.FindRunningTask(ctx, ev.ChatID, ev.SenderOpenID)
	if err != nil {
		return err
	}
	if ok {
		return r.notifier.RunningConflict(ctx, notify.RunningConflictInput{
			ChatID:           ev.ChatID,
			ReplyToMessageID: ev.MessageID,
			TaskID:           running.ID,
			Status:           running.Status,
			ProjectAlias:     running.ProjectAlias,
		})
	}
	project, err := r.cfg.ResolveProject(alias)
	if err != nil {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "Project configuration error: "+err.Error())
	}
	taskID := r.newTaskID()
	runID := r.newRunID()
	admit, err := r.store.AdmitNewTask(ctx, ev.DedupKey, sourceFor(ev), store.CreateTaskInput{
		TaskID:                         taskID,
		RunID:                          runID,
		ProjectAlias:                   alias,
		CWD:                            project.CWD,
		CreatedBy:                      ev.SenderOpenID,
		ChatID:                         ev.ChatID,
		Prompt:                         prompt,
		EffectiveCodexCommand:          project.Command,
		EffectiveSandbox:               project.Sandbox,
		EffectiveModel:                 project.Model,
		EffectiveApproval:              project.Approval,
		EffectiveApprovalFlagSupported: false,
		EffectiveExtraArgs:             project.ExtraArgs,
		Now:                            r.now(),
	})
	if err != nil || admit.Replay {
		return err
	}
	start, err := r.notifier.Start(ctx, cardInput(admit.Task, "running", "Task accepted.", ""))
	if err != nil || start.MessageID == "" {
		if err == nil {
			err = notify.ErrMissingMessageID
		}
		_ = r.store.FinishRun(ctx, ev.DedupKey, admit.Run.ID, contracts.RunResult{ExitCode: -1, FinalText: err.Error(), FinishedAt: r.now()}, "failed")
		return nil
	}
	if err := r.insertRouteWithRetry(ctx, start.MessageID, admit.Task.ID, "start_card"); err != nil {
		_ = r.store.FinishRun(ctx, ev.DedupKey, admit.Run.ID, contracts.RunResult{ExitCode: -1, FinalText: err.Error(), FinishedAt: r.now()}, "failed")
		return nil
	}
	result, runErr := r.runner.Exec(ctx, execInput(admit.Task, admit.Run.ID, prompt, func(threadID string) error {
		return r.store.RecordRunSession(ctx, admit.Run.ID, threadID)
	}))
	status := statusFromError(runErr)
	if err := r.store.FinishRun(ctx, ev.DedupKey, admit.Run.ID, result, status); err != nil {
		return err
	}
	return r.sendResult(ctx, admit.Task, status, result, runErr)
}

func (r *Router) handleContinuation(ctx context.Context, ev contracts.InboundEvent) error {
	text := strings.TrimSpace(ev.Text)
	if ev.Kind == contracts.InboundCardAction && text == "" {
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "Follow-up text is required.")
	}
	task, err := r.store.ResolveMessageRoute(ctx, ev.RootMessageID)
	if errors.Is(err, store.ErrRouteMiss) {
		_, sendErr := r.notifier.RoutingError(ctx, ev.ChatID, ev.MessageID)
		return sendErr
	}
	if err != nil {
		return err
	}
	runID := r.newRunID()
	admit, err := r.store.AdmitResumeRun(ctx, ev.DedupKey, sourceFor(ev), store.ResumeRunInput{RunID: runID, TaskID: task.ID, RequestedBy: ev.SenderOpenID, Prompt: text, Now: r.now()})
	if err != nil || admit.Replay {
		return err
	}
	switch admit.Reason {
	case store.RejectNone:
	case store.RejectCreatorMismatch:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "Only the task creator can continue this task.")
	case store.RejectActiveRun:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "This task is already running.")
	case store.RejectMissingSession:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "This task has no Codex session id to resume.")
	default:
		return r.notifier.Rejection(ctx, ev.ChatID, ev.MessageID, "This task cannot be resumed.")
	}
	result, runErr := r.runner.Resume(ctx, codexrunner.ResumeInput{
		ExecInput: execInput(admit.Task, admit.Run.ID, text, func(threadID string) error {
			return r.store.RecordRunSession(ctx, admit.Run.ID, threadID)
		}),
		SessionID: admit.Task.CodexSessionID,
		Reply:     text,
	})
	status := statusFromError(runErr)
	if err := r.store.FinishRun(ctx, ev.DedupKey, admit.Run.ID, result, status); err != nil {
		return err
	}
	return r.sendResult(ctx, admit.Task, status, result, runErr)
}

func (r *Router) sendResult(ctx context.Context, task store.Task, status string, result contracts.RunResult, runErr error) error {
	body := result.FinalText
	if runErr != nil {
		body = runErr.Error()
		if result.StderrTail != "" {
			body += "\n" + result.StderrTail
		}
	}
	input := cardInput(task, status, body, result.CodexSessionID)
	var sent contracts.SentMessage
	var err error
	if status == "succeeded" {
		sent, err = r.notifier.Success(ctx, input)
	} else {
		sent, err = r.notifier.Failure(ctx, input)
	}
	if err != nil || sent.MessageID == "" {
		return nil
	}
	_ = r.insertRouteWithRetry(ctx, sent.MessageID, task.ID, "result_card")
	return nil
}

func (r *Router) insertRouteWithRetry(ctx context.Context, messageID, taskID, routeType string) error {
	err := r.store.InsertMessageRoute(ctx, messageID, taskID, routeType)
	if err == nil {
		return nil
	}
	return r.store.InsertMessageRoute(ctx, messageID, taskID, routeType)
}

func (r *Router) authorized(ctx context.Context, openID string) bool {
	allowed := false
	for _, id := range r.cfg.Security.AllowedOpenIDs {
		if id == openID {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	_, _ = r.store.UserEnabled(ctx, openID)
	return true
}

func execInput(task store.Task, runID, prompt string, onSessionID func(string) error) codexrunner.ExecInput {
	return codexrunner.ExecInput{
		Command:               task.EffectiveCodexCommand,
		CWD:                   task.CWD,
		Sandbox:               task.EffectiveSandbox,
		Model:                 task.EffectiveModel,
		Approval:              task.EffectiveApproval,
		ApprovalFlagSupported: task.EffectiveApprovalFlagSupported,
		ExtraArgs:             task.EffectiveExtraArgs,
		Prompt:                prompt,
		TaskID:                task.ID,
		RunID:                 runID,
		OnSessionID:           onSessionID,
	}
}

func cardInput(task store.Task, status, body, sessionID string) notify.TaskCardInput {
	return notify.TaskCardInput{
		ChatID:         task.ChatID,
		TaskID:         task.ID,
		Status:         status,
		ProjectAlias:   task.ProjectAlias,
		CWDLabel:       task.CWD,
		Body:           body,
		CodexSessionID: sessionID,
	}
}

func statusFromError(err error) string {
	if err != nil {
		return "failed"
	}
	return "succeeded"
}

func sourceFor(ev contracts.InboundEvent) string {
	if ev.Kind == contracts.InboundCardAction {
		return "card_callback"
	}
	return "message"
}

func containsProjectAlias(aliases []string, want string) bool {
	for _, alias := range aliases {
		if alias == want {
			return true
		}
	}
	return false
}

func randomTaskID() string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("cx_%d", time.Now().UnixNano())
	}
	return "cx_" + hex.EncodeToString(buf[:])
}

func randomPendingID() string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("pi_%d", time.Now().UnixNano())
	}
	return "pi_" + hex.EncodeToString(buf[:])
}
