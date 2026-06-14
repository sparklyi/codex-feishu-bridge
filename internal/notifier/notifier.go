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
	return n.sendTask(ctx, contracts.CardStart, "Codex task started", in, successBodyLimit)
}

func (n *Notifier) Success(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardSuccess, "Codex task succeeded", in, successBodyLimit)
}

func (n *Notifier) Failure(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardFailure, "Codex task failed", in, failureBodyLimit)
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

func (n *Notifier) sendTask(ctx context.Context, kind contracts.CardKind, title string, in TaskCardInput, limit int) (contracts.SentMessage, error) {
	body := buildBody(in, limit)
	msg := contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         kind,
		TaskID:           in.TaskID,
		Status:           in.Status,
		Title:            redact.FeishuText(title+" "+in.TaskID, 120),
		BodyMarkdown:     body,
		Fields:           taskFields(in),
		Actions:          taskActions(in.TaskID),
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
		{Title: "Status", Value: in.Status},
		{Title: "Project", Value: project},
		{Title: "Workspace", Value: redact.FeishuText(in.CWDLabel, 200)},
	}
}

func taskActions(taskID string) []contracts.Action {
	return []contracts.Action{
		{ID: continueActionID, Label: "Continue", Style: "primary", Value: map[string]string{"action": "continue", "task_id": taskID}},
		{ID: "shortcut", Label: "Summarize", Value: map[string]string{"action": "shortcut", "shortcut": "summarize", "task_id": taskID}},
		{ID: "shortcut", Label: "Explain error", Value: map[string]string{"action": "shortcut", "shortcut": "explain_error", "task_id": taskID}},
		{ID: "shortcut", Label: "Run tests", Value: map[string]string{"action": "shortcut", "shortcut": "run_tests", "task_id": taskID}},
		{ID: "shortcut", Label: "MR description", Value: map[string]string{"action": "shortcut", "shortcut": "mr_description", "task_id": taskID}},
	}
}

func buildBody(in TaskCardInput, limit int) string {
	project := in.ProjectAlias
	if project == "" {
		project = "default"
	}
	body := in.Body
	if in.CodexSessionID != "" {
		body = strings.ReplaceAll(body, in.CodexSessionID, "[codex-session]")
	}
	text := strings.Join([]string{
		"Task: " + in.TaskID,
		"Status: " + in.Status,
		"Project: " + project,
		"Workspace: " + in.CWDLabel,
		"",
		body,
	}, "\n")
	return redact.FeishuText(text, limit)
}
