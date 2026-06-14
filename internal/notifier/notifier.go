package notifier

import (
	"context"
	"errors"
	"strings"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/redact"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

const (
	continueActionID = "continue_submit"
	successBodyLimit = 4000
	failureBodyLimit = 2000
)

var ErrMissingMessageID = errors.New("routeable card send returned empty message id")

type Notifier struct {
	sender transport.Sender
}

type TaskCardInput struct {
	ChatID           string
	ReplyToMessageID string
	TaskID           string
	Status           string
	ProjectAlias     string
	CWDLabel         string
	Body             string
	CodexSessionID   string
}

type ProjectSelectionInput struct {
	ChatID           string
	ReplyToMessageID string
	PendingID        string
	Prompt           string
	ProjectAliases   []string
}

type RunningConflictInput struct {
	ChatID           string
	ReplyToMessageID string
	TaskID           string
	Status           string
	ProjectAlias     string
}

type ShortcutConfirmationInput struct {
	ChatID           string
	ReplyToMessageID string
	RootMessageID    string
	Shortcut         string
	Prompt           string
}

func New(sender transport.Sender) *Notifier {
	return &Notifier{sender: sender}
}

func (n *Notifier) Start(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardStart, in, successBodyLimit)
}

func (n *Notifier) Success(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardSuccess, in, successBodyLimit)
}

func (n *Notifier) Failure(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardFailure, in, failureBodyLimit)
}

func (n *Notifier) RoutingError(ctx context.Context, chatID, replyToMessageID string) (contracts.SentMessage, error) {
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           chatID,
		ReplyToMessageID: replyToMessageID,
		CardKind:         contracts.CardRoutingError,
		Status:           "routing_error",
		Title:            "Cannot route reply",
		BodyMarkdown:     "Please reply from a task card or start a new task in a private chat.",
	})
}

func (n *Notifier) MigrationHint(ctx context.Context, chatID, replyToMessageID string) error {
	_, err := n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           chatID,
		ReplyToMessageID: replyToMessageID,
		CardKind:         contracts.CardMigrationHint,
		Status:           "migration_hint",
		Title:            "Command updated",
		BodyMarkdown:     "Send plain text in a private chat to start a Codex task. Use `@project prompt` when you need a configured project.",
	})
	return err
}

func (n *Notifier) ProjectSelection(ctx context.Context, in ProjectSelectionInput) (contracts.SentMessage, error) {
	body := "Prompt: " + redact.FeishuText(in.Prompt, 500)
	if len(in.ProjectAliases) > 0 {
		body += "\nProjects: " + strings.Join(in.ProjectAliases, ", ")
	}
	actions := make([]contracts.Action, 0, len(in.ProjectAliases))
	for _, alias := range in.ProjectAliases {
		actions = append(actions, contracts.Action{
			ID:    "project_select",
			Label: alias,
			Value: map[string]string{
				"action":     "select_project",
				"pending_id": in.PendingID,
				"project":    alias,
			},
		})
	}
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         contracts.CardProjectSelection,
		Status:           "project_selection",
		Title:            "Choose project",
		BodyMarkdown:     body,
		Actions:          actions,
	})
}

func (n *Notifier) RunningConflict(ctx context.Context, in RunningConflictInput) error {
	project := in.ProjectAlias
	if project == "" {
		project = "default"
	}
	_, err := n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         contracts.CardRunningConflict,
		TaskID:           in.TaskID,
		Status:           "running_conflict",
		Title:            "Task already running",
		BodyMarkdown:     "Task: " + in.TaskID + "\nStatus: " + in.Status + "\nProject: " + project,
		Fields: []contracts.Field{
			{Title: "Task", Value: in.TaskID},
			{Title: "Status", Value: in.Status},
			{Title: "Project", Value: project},
		},
	})
	return err
}

func (n *Notifier) ShortcutConfirmation(ctx context.Context, in ShortcutConfirmationInput) (contracts.SentMessage, error) {
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         contracts.CardShortcutConfirm,
		Status:           "shortcut_confirmation",
		Title:            "Confirm shortcut",
		BodyMarkdown:     redact.FeishuText(in.Prompt, failureBodyLimit),
		Actions: []contracts.Action{
			{ID: "confirm_shortcut", Label: "Run", Style: "primary", Value: map[string]string{"action": "confirm_shortcut", "shortcut": in.Shortcut, "root_message_id": in.RootMessageID}},
			{ID: "cancel_shortcut", Label: "Cancel", Value: map[string]string{"action": "cancel_shortcut", "shortcut": in.Shortcut}},
		},
	})
}

func (n *Notifier) Rejection(ctx context.Context, chatID, replyToMessageID, body string) error {
	_, err := n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           chatID,
		ReplyToMessageID: replyToMessageID,
		CardKind:         contracts.CardRoutingError,
		Status:           "rejected",
		Title:            "Request rejected",
		BodyMarkdown:     redact.FeishuText(body, failureBodyLimit),
	})
	return err
}

func (n *Notifier) sendTask(ctx context.Context, kind contracts.CardKind, in TaskCardInput, limit int) (contracts.SentMessage, error) {
	body := buildTaskBody(kind, in, limit)
	msg := contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         kind,
		TaskID:           in.TaskID,
		Status:           in.Status,
		Title:            redact.FeishuText(taskTitle(kind, in.TaskID), 120),
		BodyMarkdown:     body,
		Fields:           taskFields(in),
		Actions:          taskActions(kind, in.TaskID),
	}
	sent, err := n.sender.Send(ctx, msg)
	if err != nil {
		return contracts.SentMessage{}, err
	}
	if sent.MessageID == "" {
		return contracts.SentMessage{}, ErrMissingMessageID
	}
	return sent, nil
}

func taskFields(in TaskCardInput) []contracts.Field {
	project := in.ProjectAlias
	if project == "" {
		project = "default"
	}
	return []contracts.Field{
		{Title: "状态", Value: localizedStatus(in.Status)},
		{Title: "项目", Value: project},
		{Title: "工作区", Value: redact.FeishuText(in.CWDLabel, 200)},
	}
}

func taskActions(_ contracts.CardKind, taskID string) []contracts.Action {
	return []contracts.Action{
		{ID: continueActionID, Label: "继续跟进", Style: "primary", Value: map[string]string{"action": "continue", "task_id": taskID}},
	}
}

func buildTaskBody(kind contracts.CardKind, in TaskCardInput, limit int) string {
	body := in.Body
	if in.CodexSessionID != "" {
		body = strings.ReplaceAll(body, in.CodexSessionID, "[codex-session]")
	}
	body = strings.TrimSpace(body)
	if kind == contracts.CardStart || body == "" || body == "Task accepted." {
		body = "已接收，Codex 正在处理。"
	}
	text := "**" + bodyHeading(kind) + "**\n" + body
	if kind != contracts.CardStart {
		text += "\n\n继续处理请直接回复这张任务卡片。"
	}
	return redact.FeishuText(text, limit)
}

func taskTitle(kind contracts.CardKind, taskID string) string {
	prefix := "任务状态"
	switch kind {
	case contracts.CardStart:
		prefix = "正在处理"
	case contracts.CardSuccess:
		prefix = "任务已完成"
	case contracts.CardFailure:
		prefix = "任务失败"
	}
	if taskID == "" {
		return prefix
	}
	return prefix + " · " + taskID
}

func bodyHeading(kind contracts.CardKind) string {
	switch kind {
	case contracts.CardStart:
		return "进度"
	case contracts.CardSuccess:
		return "结果"
	case contracts.CardFailure:
		return "错误"
	default:
		return "摘要"
	}
}

func localizedStatus(status string) string {
	switch status {
	case "running":
		return "运行中"
	case "succeeded":
		return "已完成"
	case "failed":
		return "失败"
	case "":
		return "未知"
	default:
		return status
	}
}
