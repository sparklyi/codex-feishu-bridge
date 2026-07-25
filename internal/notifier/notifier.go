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
	UpdateMessageID  string
	TaskID           string
	Status           string
	ProjectAlias     string
	CWDLabel         string
	Body             string
}

type ThreadOption struct {
	ID      string
	Name    string
	Preview string
	CWD     string
	Status  string
}

type ThreadSelectionInput struct {
	ChatID           string
	ReplyToMessageID string
	Threads          []ThreadOption
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

func New(sender transport.Sender) *Notifier {
	return &Notifier{sender: sender}
}

func (n *Notifier) Start(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardStart, in, successBodyLimit)
}

func (n *Notifier) Progress(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardStart, in, successBodyLimit)
}

func (n *Notifier) Success(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardSuccess, in, successBodyLimit)
}

func (n *Notifier) Failure(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardFailure, in, failureBodyLimit)
}

func (n *Notifier) ThreadSelection(ctx context.Context, in ThreadSelectionInput) (contracts.SentMessage, error) {
	if len(in.Threads) == 0 {
		return n.sender.Send(ctx, contracts.OutboundMessage{
			ChatID:           in.ChatID,
			ReplyToMessageID: in.ReplyToMessageID,
			CardKind:         contracts.CardThreadSelection,
			Status:           "empty",
			Title:            "没有可接管的会话",
			BodyMarkdown:     "本机 Codex 暂未发现可用会话。",
		})
	}
	actions := make([]contracts.Action, 0, len(in.Threads))
	lines := make([]string, 0, len(in.Threads)+1)
	lines = append(lines, "选择一个本机 Codex 会话以从飞书继续处理。")
	for index, thread := range in.Threads {
		label := threadLabel(thread, index)
		lines = append(lines, "**"+label+"**\n"+redact.FeishuText(thread.Preview, 180))
		actions = append(actions, contracts.Action{
			ID:    "attach_thread",
			Label: label,
			Value: map[string]string{"action": "attach_thread", "thread_id": thread.ID},
		})
	}
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         contracts.CardThreadSelection,
		Status:           "select_thread",
		Title:            "接管 Codex 会话",
		BodyMarkdown:     redact.FeishuText(strings.Join(lines, "\n\n"), successBodyLimit),
		Actions:          actions,
	})
}

func (n *Notifier) RoutingError(ctx context.Context, chatID, replyToMessageID string) (contracts.SentMessage, error) {
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           chatID,
		ReplyToMessageID: replyToMessageID,
		CardKind:         contracts.CardRoutingError,
		Status:           "routing_error",
		Title:            "无法定位任务",
		BodyMarkdown:     "请从任务卡片继续，或重新发起一个任务。",
	})
}

func (n *Notifier) ProjectSelection(ctx context.Context, in ProjectSelectionInput) (contracts.SentMessage, error) {
	body := "任务：" + redact.FeishuText(in.Prompt, 500)
	actions := make([]contracts.Action, 0, len(in.ProjectAliases))
	for _, alias := range in.ProjectAliases {
		actions = append(actions, contracts.Action{
			ID:    "project_select",
			Label: alias,
			Value: map[string]string{"action": "select_project", "pending_id": in.PendingID, "project": alias},
		})
	}
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         contracts.CardProjectSelection,
		Status:           "project_selection",
		Title:            "选择项目",
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
		Title:            "任务仍在处理",
		BodyMarkdown:     "请等待当前操作完成后再继续。",
		Fields: []contracts.Field{
			{Title: "任务", Value: in.TaskID},
			{Title: "状态", Value: localizedStatus(in.Status)},
			{Title: "项目", Value: project},
		},
	})
	return err
}

func (n *Notifier) Rejection(ctx context.Context, chatID, replyToMessageID, body string) error {
	_, err := n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           chatID,
		ReplyToMessageID: replyToMessageID,
		CardKind:         contracts.CardRoutingError,
		Status:           "rejected",
		Title:            "请求未执行",
		BodyMarkdown:     redact.FeishuText(body, failureBodyLimit),
	})
	return err
}

func (n *Notifier) sendTask(ctx context.Context, kind contracts.CardKind, in TaskCardInput, limit int) (contracts.SentMessage, error) {
	msg := contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		UpdateMessageID:  in.UpdateMessageID,
		CardKind:         kind,
		TaskID:           in.TaskID,
		Status:           in.Status,
		Title:            redact.FeishuText(taskTitle(kind, in.Status, in.TaskID), 120),
		BodyMarkdown:     buildTaskBody(kind, in, limit),
		Fields:           taskFields(in),
		Actions:          taskActions(in.Status, in.TaskID),
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

func taskActions(status, taskID string) []contracts.Action {
	actions := []contracts.Action{{ID: continueActionID, Label: "继续跟进", Style: "primary", Value: map[string]string{"action": "continue", "task_id": taskID}}}
	if status == "queued" || status == "running" {
		actions = append(actions, contracts.Action{ID: "stop_task", Label: "停止", Style: "danger", Value: map[string]string{"action": "stop_task", "task_id": taskID}})
	}
	return actions
}

func buildTaskBody(kind contracts.CardKind, in TaskCardInput, limit int) string {
	body := strings.TrimSpace(in.Body)
	if body == "" {
		switch in.Status {
		case "idle":
			body = "已绑定桌面 Codex 会话。"
		case "queued":
			body = "任务已进入队列。"
		default:
			body = "Codex 正在处理。"
		}
	}
	text := "**" + bodyHeading(kind) + "**\n" + body
	return redact.FeishuText(text, limit)
}

func threadLabel(thread ThreadOption, index int) string {
	label := strings.TrimSpace(thread.Name)
	if label == "" {
		label = strings.TrimSpace(thread.Preview)
	}
	if label == "" {
		label = "会话 " + string(rune('A'+index))
	}
	return redact.FeishuText(label, 42)
}

func taskTitle(kind contracts.CardKind, status, taskID string) string {
	prefix := "任务状态"
	switch {
	case kind == contracts.CardSuccess:
		prefix = "任务已完成"
	case kind == contracts.CardFailure:
		prefix = "任务未完成"
	case status == "idle":
		prefix = "已接管会话"
	case status == "queued":
		prefix = "等待处理"
	default:
		prefix = "正在处理"
	}
	if taskID == "" {
		return prefix
	}
	return prefix + " · " + taskID
}

func bodyHeading(kind contracts.CardKind) string {
	switch kind {
	case contracts.CardSuccess:
		return "结果"
	case contracts.CardFailure:
		return "错误"
	default:
		return "进度"
	}
}

func localizedStatus(status string) string {
	switch status {
	case "idle":
		return "已接管"
	case "queued":
		return "排队中"
	case "running":
		return "运行中"
	case "succeeded":
		return "已完成"
	case "failed":
		return "失败"
	case "canceled":
		return "已停止"
	default:
		return status
	}
}
