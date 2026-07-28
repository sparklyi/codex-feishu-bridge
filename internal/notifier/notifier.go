package notifier

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/redact"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

const (
	continueActionID = "continue_submit"
	steerActionID    = "steer_submit"
	successBodyLimit = 4000
	failureBodyLimit = 2000
	// Keep enough output for a useful live preview while leaving room for the
	// rest of the card under Feishu's card-size limit.
	processingDetailLimit = 6 * 1024
	// Progress updates are superseded by newer card state, so the runtime owns
	// retry scheduling instead of letting a stale patch retry in the sender.
	progressDeliveryMaxAttempts = 1
)

var ErrMissingMessageID = errors.New("routeable card send returned empty message id")

type Notifier struct {
	sender          transport.Sender
	cardDisplayMode string
}

type Options struct {
	CardDisplayMode string
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
	UserInputs       []string
	Presentation     contracts.TaskPresentation
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

type RunningConflictInput struct {
	ChatID           string
	ReplyToMessageID string
	TaskID           string
	Status           string
	ProjectAlias     string
}

func New(sender transport.Sender, options ...Options) *Notifier {
	mode := "preview"
	if len(options) > 0 && options[0].CardDisplayMode == "concise" {
		mode = "concise"
	}
	return &Notifier{sender: sender, cardDisplayMode: mode}
}

func (n *Notifier) Start(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardStart, in, successBodyLimit, 0)
}

func (n *Notifier) Progress(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardStart, in, successBodyLimit, progressDeliveryMaxAttempts)
}

func (n *Notifier) Success(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardSuccess, in, successBodyLimit, 0)
}

func (n *Notifier) Failure(ctx context.Context, in TaskCardInput) (contracts.SentMessage, error) {
	return n.sendTask(ctx, contracts.CardFailure, in, failureBodyLimit, 0)
}

// Restarting confirms a native bridge restart before the service tears down
// its app-server child process. This path intentionally does not create a
// Codex task or task card.
func (n *Notifier) Restarting(ctx context.Context, chatID, replyToMessageID string) error {
	_, err := n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           chatID,
		ReplyToMessageID: replyToMessageID,
		CardKind:         contracts.CardRestarting,
		Status:           "restarting",
		Title:            "服务正在重启",
		Subtitle:         "正在重新连接 Feishu 与 Codex",
		BodyMarkdown:     "已确认重启请求。服务将在片刻后恢复。",
	})
	return err
}

func (n *Notifier) ThreadSelection(ctx context.Context, in ThreadSelectionInput) (contracts.SentMessage, error) {
	if len(in.Threads) == 0 {
		return n.sender.Send(ctx, contracts.OutboundMessage{
			ChatID:           in.ChatID,
			ReplyToMessageID: in.ReplyToMessageID,
			CardKind:         contracts.CardThreadSelection,
			Status:           "empty",
			Title:            "没有可接管的会话",
			Subtitle:         "本机 Codex 暂未发现可用会话",
		})
	}
	options := make([]contracts.CardOption, 0, len(in.Threads))
	for index, thread := range in.Threads {
		label := threadLabel(thread, index)
		meta := ""
		if thread.Status != "" {
			meta = "状态：" + localizedStatus(thread.Status)
		}
		options = append(options, contracts.CardOption{
			Title:  label,
			Detail: redact.FeishuText(thread.Preview, 180),
			Meta:   meta,
			Action: contracts.Action{
				ID:    "attach_thread",
				Label: "接管",
				Style: "primary",
				Value: map[string]string{"action": "attach_thread", "thread_id": thread.ID},
			},
		})
	}
	return n.sender.Send(ctx, contracts.OutboundMessage{
		ChatID:           in.ChatID,
		ReplyToMessageID: in.ReplyToMessageID,
		CardKind:         contracts.CardThreadSelection,
		Status:           "select_thread",
		Title:            "选择要接管的会话",
		Subtitle:         "从桌面 Codex 会话继续",
		BodyMarkdown:     "可接管的会话",
		Options:          options,
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

func (n *Notifier) sendTask(ctx context.Context, kind contracts.CardKind, in TaskCardInput, limit, deliveryMaxAttempts int) (contracts.SentMessage, error) {
	presentation := normalizeTaskPresentation(kind, in, n.cardDisplayMode, limit)
	msg := contracts.OutboundMessage{
		ChatID:              in.ChatID,
		ReplyToMessageID:    in.ReplyToMessageID,
		UpdateMessageID:     in.UpdateMessageID,
		DeliveryMaxAttempts: deliveryMaxAttempts,
		CardKind:            kind,
		TaskID:              in.TaskID,
		Status:              in.Status,
		Title:               taskTitle(kind, in.Status),
		Subtitle:            taskSubtitle(in),
		Presentation:        &presentation,
		StreamDetail:        kind == contracts.CardStart && n.cardDisplayMode == "preview" && (in.Status == "queued" || in.Status == "running"),
		Actions:             taskActions(in.Status, in.TaskID),
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

func normalizeTaskPresentation(kind contracts.CardKind, in TaskCardInput, displayMode string, limit int) contracts.TaskPresentation {
	presentation := in.Presentation
	if len(presentation.UserInputs) == 0 && len(in.UserInputs) > 0 {
		presentation.UserInputs = append([]string(nil), in.UserInputs...)
	}
	if kind == contracts.CardSuccess || kind == contracts.CardFailure {
		presentation.Layout = contracts.TaskCardResult
		if presentation.Conclusion == "" {
			presentation.Conclusion = taskBody(in)
		}
	} else {
		presentation.Layout = contracts.TaskCardRunning
		if presentation.Stage == "" {
			presentation.Stage = stageForStatus(in.Status)
		}
		if presentation.Activity == "" {
			presentation.Activity = taskBody(in)
		}
		if displayMode != "preview" {
			presentation.ProcessingDetail = ""
		}
	}
	return redactPresentation(presentation, limit)
}

func redactPresentation(presentation contracts.TaskPresentation, limit int) contracts.TaskPresentation {
	presentation.Stage = redact.FeishuText(strings.TrimSpace(presentation.Stage), 120)
	presentation.Activity = redact.FeishuText(strings.TrimSpace(presentation.Activity), 240)
	presentation.ProcessingDetail = redactProcessingDetail(presentation.ProcessingDetail)
	presentation.Conclusion = redact.FeishuText(strings.TrimSpace(presentation.Conclusion), limit)
	presentation.UserInputs = redactUserInputs(presentation.UserInputs)
	presentation.Milestones = redactMilestones(presentation.Milestones)
	presentation.Changes = redactItems(presentation.Changes, 5, 220)
	presentation.Verification = redactItems(presentation.Verification, 5, 220)
	return presentation
}

func redactProcessingDetail(value string) string {
	// CardKit streams only when each value extends the previous one. Preserve
	// the leading text at the display limit instead of rotating in a suffix.
	return redact.FeishuText(strings.TrimSpace(value), processingDetailLimit)
}

func redactUserInputs(values []string) []string {
	const (
		maxItems  = 10
		itemLimit = 800
	)
	if len(values) > maxItems {
		// The original request anchors the current turn, so retain it alongside
		// the most recent steering inputs when the card must be compacted.
		compacted := make([]string, 0, maxItems)
		compacted = append(compacted, values[0])
		compacted = append(compacted, values[len(values)-(maxItems-1):]...)
		values = compacted
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = redact.FeishuText(strings.TrimSpace(value), itemLimit)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func redactMilestones(values []contracts.TaskMilestone) []contracts.TaskMilestone {
	if len(values) == 0 {
		return nil
	}
	if len(values) > 5 {
		values = values[len(values)-5:]
	}
	result := make([]contracts.TaskMilestone, 0, len(values))
	for _, value := range values {
		label := redact.FeishuText(strings.TrimSpace(value.Label), 180)
		if label == "" {
			continue
		}
		result = append(result, contracts.TaskMilestone{Label: label, Kind: value.Kind})
	}
	return result
}

func redactItems(values []string, maxItems, limit int) []string {
	if len(values) == 0 {
		return nil
	}
	if len(values) > maxItems {
		values = values[len(values)-maxItems:]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = redact.FeishuText(strings.TrimSpace(value), limit)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func projectLabel(alias, cwd string) string {
	if alias = strings.TrimSpace(alias); alias != "" {
		return alias
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		base := filepath.Base(filepath.Clean(cwd))
		if base != "." && base != string(filepath.Separator) {
			return redact.FeishuText(base, 80)
		}
	}
	return "default"
}

func taskSubtitle(in TaskCardInput) string {
	return "项目：" + projectLabel(in.ProjectAlias, in.CWDLabel)
}

func taskActions(status, taskID string) []contracts.Action {
	if status == "queued" {
		return []contracts.Action{{ID: "stop_task", Label: "停止", Style: "danger", Value: map[string]string{"action": "stop_task", "task_id": taskID}}}
	}
	if status == "running" {
		return []contracts.Action{
			{ID: steerActionID, Label: "补充到本轮", Style: "primary", Value: map[string]string{"action": "steer", "task_id": taskID}},
			{ID: "stop_task", Label: "停止", Style: "danger", Value: map[string]string{"action": "stop_task", "task_id": taskID}},
		}
	}
	return []contracts.Action{{ID: continueActionID, Label: "继续跟进", Style: "primary", Value: map[string]string{"action": "continue", "task_id": taskID}}}
}

func taskBody(in TaskCardInput) string {
	body := strings.TrimSpace(in.Body)
	if body != "" {
		return body
	}
	switch in.Status {
	case "idle":
		return "已绑定桌面 Codex 会话。"
	case "queued":
		return "任务已进入队列。"
	case "succeeded":
		return "任务已完成。"
	case "failed":
		return "任务未完成。"
	case "canceled":
		return "任务已停止。"
	default:
		return "Codex 正在处理。"
	}
}

func stageForStatus(status string) string {
	switch status {
	case "queued":
		return "等待执行"
	case "idle":
		return "会话已接管"
	default:
		return "执行中"
	}
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

func taskTitle(kind contracts.CardKind, status string) string {
	var prefix string
	switch {
	case status == "canceled":
		prefix = "任务已停止"
	case kind == contracts.CardSuccess:
		prefix = "任务已完成"
	case kind == contracts.CardFailure:
		prefix = "任务未完成"
	case status == "idle":
		prefix = "已接管会话"
	case status == "queued":
		prefix = "等待处理"
	case status == "failed":
		prefix = "任务未完成"
	default:
		prefix = "正在处理"
	}
	return prefix
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
