package notifier

import (
	"context"
	"errors"
	"strings"

	"github.com/sihuo/codex-feishu-bridge/internal/contracts"
	"github.com/sihuo/codex-feishu-bridge/internal/redact"
	"github.com/sihuo/codex-feishu-bridge/internal/transport"
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
		BodyMarkdown:     "Please reply from a task card or start a new `/codex` task.",
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
		Actions:          []contracts.Action{{ID: continueActionID, Label: "Continue"}},
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
