package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

// ErrRateLimited is retained for callers of the Feishu transport. It is the
// shared transport sentinel so runtime delivery retries classify it correctly.
var ErrRateLimited = transport.ErrRateLimited

const defaultDeliveryAttemptTimeout = 5 * time.Second

type CardAPI interface {
	SendCard(ctx context.Context, chatID, replyToMessageID string, cardJSON []byte) (messageID string, retryAfter time.Duration, err error)
	PatchCard(ctx context.Context, messageID string, cardJSON []byte) (retryAfter time.Duration, err error)
}

type Sender struct {
	AppID          string
	AppSecret      string
	API            CardAPI
	MaxRetries     int
	AttemptTimeout time.Duration
	Sleep          func(context.Context, time.Duration) error
}

func NewSenderFromEnv(appID, secretEnv string, getenv func(string) string, api CardAPI) (*Sender, error) {
	if getenv == nil {
		return nil, errors.New("getenv is required")
	}
	secret := getenv(secretEnv)
	if secret == "" {
		return nil, fmt.Errorf("missing Feishu app secret env %s", secretEnv)
	}
	return &Sender{AppID: appID, AppSecret: secret, API: api}, nil
}

func NewSDKCardAPI(appID, appSecret string, proxyURL *url.URL) *SDKCardAPI {
	return &SDKCardAPI{client: lark.NewClient(appID, appSecret, lark.WithHttpClient(newFeishuHTTPClient(feishuHTTPTimeout, proxyURL)))}
}

type SDKCardAPI struct {
	client *lark.Client
}

func (api *SDKCardAPI) SendCard(ctx context.Context, chatID, replyToMessageID string, cardJSON []byte) (string, time.Duration, error) {
	content := string(cardJSON)
	if replyToMessageID != "" {
		body := larkim.NewReplyMessageReqBodyBuilder().
			MsgType("interactive").
			Content(content).
			Build()
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(replyToMessageID).
			Body(body).
			Build()
		resp, err := api.client.Im.Message.Reply(ctx, req)
		if err != nil {
			return "", 0, err
		}
		if !resp.Success() {
			return "", 0, feishuResponseError("reply", resp.Code, resp.Msg)
		}
		if resp.Data == nil || resp.Data.MessageId == nil {
			return "", 0, nil
		}
		return *resp.Data.MessageId, 0, nil
	}
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(chatID).
		MsgType("interactive").
		Content(content).
		Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(body).
		Build()
	resp, err := api.client.Im.Message.Create(ctx, req)
	if err != nil {
		return "", 0, err
	}
	if !resp.Success() {
		return "", 0, feishuResponseError("send", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", 0, nil
	}
	return *resp.Data.MessageId, 0, nil
}

func (api *SDKCardAPI) PatchCard(ctx context.Context, messageID string, cardJSON []byte) (time.Duration, error) {
	body := larkim.NewPatchMessageReqBodyBuilder().
		Content(string(cardJSON)).
		Build()
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()
	resp, err := api.client.Im.Message.Patch(ctx, req)
	if err != nil {
		return 0, err
	}
	if !resp.Success() {
		return 0, feishuResponseError("patch", resp.Code, resp.Msg)
	}
	return 0, nil
}

func (s *Sender) Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error) {
	if s.API == nil {
		return contracts.SentMessage{}, errors.New("feishu sender API is nil")
	}
	card, err := BuildInteractiveCard(msg)
	if err != nil {
		return contracts.SentMessage{}, err
	}
	if msg.UpdateMessageID != "" {
		if err := s.patchWithRetry(ctx, msg.UpdateMessageID, card); err != nil {
			return contracts.SentMessage{}, err
		}
		return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
	}
	maxRetries := s.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptCtx, cancel := s.attemptContext(ctx)
		messageID, retryAfter, err := s.API.SendCard(attemptCtx, msg.ChatID, msg.ReplyToMessageID, card)
		cancel()
		if err == nil {
			if messageID == "" {
				return contracts.SentMessage{}, errors.New("Feishu send returned empty message id")
			}
			return contracts.SentMessage{MessageID: messageID}, nil
		}
		lastErr = err
		if !shouldRetrySendError(err) || attempt == maxRetries {
			return contracts.SentMessage{}, err
		}
		if retryAfter <= 0 {
			retryAfter = time.Duration(attempt+1) * 100 * time.Millisecond
		}
		if err := s.sleep(ctx, retryAfter); err != nil {
			return contracts.SentMessage{}, err
		}
	}
	return contracts.SentMessage{}, lastErr
}

func (s *Sender) patchWithRetry(ctx context.Context, messageID string, card []byte) error {
	maxRetries := s.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptCtx, cancel := s.attemptContext(ctx)
		retryAfter, err := s.API.PatchCard(attemptCtx, messageID, card)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetrySendError(err) || attempt == maxRetries {
			return err
		}
		if retryAfter <= 0 {
			retryAfter = time.Duration(attempt+1) * 100 * time.Millisecond
		}
		if err := s.sleep(ctx, retryAfter); err != nil {
			return err
		}
	}
	return lastErr
}

func shouldRetrySendError(err error) bool {
	return errors.Is(err, ErrRateLimited) || transport.IsTransientError(err)
}

func feishuResponseError(operation string, code int, message string) error {
	if isRateLimitedCode(code) {
		return fmt.Errorf("%w: Feishu %s failed: code=%d msg=%s", ErrRateLimited, operation, code, message)
	}
	return fmt.Errorf("Feishu %s failed: code=%d msg=%s", operation, code, message)
}

func isRateLimitedCode(code int) bool {
	return code == 99991400 || code == 99991401 || code == 230002
}

func (s *Sender) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.AttemptTimeout
	if timeout <= 0 {
		timeout = defaultDeliveryAttemptTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func BuildInteractiveCard(msg contracts.OutboundMessage) ([]byte, error) {
	elements := make([]any, 0, len(msg.Fields)+len(msg.Options)+3)
	if len(msg.Fields) > 0 {
		elements = append(elements, metadataGrid(msg.Fields))
	}
	if len(msg.Fields) > 0 && msg.BodyMarkdown != "" {
		elements = append(elements, map[string]any{"tag": "hr", "margin": "0px"})
	}
	if msg.BodyMarkdown != "" {
		elements = append(elements, map[string]any{
			"tag":        "markdown",
			"element_id": "card_body",
			"content":    msg.BodyMarkdown,
			"text_size":  "normal",
		})
	}
	if len(msg.Options) > 0 {
		elements = append(elements, optionRows(msg.Options)...)
	}

	var followUpAction *contracts.Action
	buttonActions := make([]contracts.Action, 0, len(msg.Actions))
	for _, action := range msg.Actions {
		if action.ID == "continue_submit" {
			actionCopy := action
			followUpAction = &actionCopy
			continue
		}
		buttonActions = append(buttonActions, action)
	}
	if len(buttonActions) > 0 {
		elements = append(elements, actionButtons(buttonActions))
	}
	if followUpAction != nil {
		elements = append(elements, followUpForm(*followUpAction))
	}

	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   "fill",
			"summary":      map[string]any{"content": cardSummary(msg)},
		},
		"header": map[string]any{
			"template":      templateFor(msg),
			"title":         map[string]any{"tag": "plain_text", "content": msg.Title},
			"subtitle":      map[string]any{"tag": "plain_text", "content": cardSubtitle(msg)},
			"text_tag_list": []any{statusTag(msg)},
			"padding":       "12px 12px 8px 12px",
		},
		"body": map[string]any{
			"direction":        "vertical",
			"padding":          "12px",
			"vertical_spacing": "12px",
			"elements":         elements,
		},
	}
	return json.Marshal(card)
}

func followUpForm(action contracts.Action) map[string]any {
	submit := actionButton(action)
	submit["name"] = action.ID
	submit["form_action_type"] = "submit"
	submit["width"] = "default"
	return map[string]any{
		"tag":              "form",
		"name":             "codex_followup_form",
		"vertical_spacing": "8px",
		"elements": []any{
			map[string]any{
				"tag":         "input",
				"name":        "text",
				"required":    true,
				"input_type":  "multiline_text",
				"rows":        2,
				"auto_resize": true,
				"max_rows":    6,
				"max_length":  1000,
				"width":       "fill",
				"label":       map[string]any{"tag": "plain_text", "content": "继续跟进"},
				"placeholder": map[string]any{"tag": "plain_text", "content": "继续补充需求或问题"},
			},
			map[string]any{
				"tag":                "column_set",
				"flex_mode":          "flow",
				"horizontal_spacing": "small",
				"columns": []any{map[string]any{
					"tag":            "column",
					"width":          "auto",
					"vertical_align": "center",
					"elements":       []any{submit},
				}},
			},
		},
	}
}

func metadataGrid(fields []contracts.Field) map[string]any {
	columns := make([]any, 0, len(fields))
	for _, field := range fields {
		columns = append(columns, map[string]any{
			"tag":              "column",
			"width":            "weighted",
			"weight":           1,
			"background_style": "grey",
			"padding":          "8px",
			"vertical_spacing": "4px",
			"elements": []any{
				plainText(field.Title, "notation", "grey"),
				plainText(field.Value, "normal", "default"),
			},
		})
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "flow",
		"horizontal_spacing": "small",
		"columns":            columns,
	}
}

func optionRows(options []contracts.CardOption) []any {
	elements := make([]any, 0, len(options))
	for _, option := range options {
		content := []any{plainText(option.Title, "heading", "default")}
		if option.Detail != "" {
			content = append(content, plainText(option.Detail, "normal", "default"))
		}
		if option.Meta != "" {
			content = append(content, plainText(option.Meta, "notation", "grey"))
		}
		elements = append(elements, map[string]any{
			"tag":                "column_set",
			"flex_mode":          "stretch",
			"background_style":   "grey",
			"horizontal_spacing": "small",
			"columns": []any{
				map[string]any{
					"tag":              "column",
					"width":            "weighted",
					"weight":           1,
					"padding":          "8px",
					"vertical_spacing": "4px",
					"elements":         content,
				},
				map[string]any{
					"tag":            "column",
					"width":          "auto",
					"padding":        "8px",
					"vertical_align": "center",
					"elements":       []any{actionButton(option.Action)},
				},
			},
		})
	}
	return elements
}

func actionButtons(actions []contracts.Action) map[string]any {
	columns := make([]any, 0, len(actions))
	for _, action := range actions {
		columns = append(columns, map[string]any{
			"tag":            "column",
			"width":          "auto",
			"vertical_align": "center",
			"elements":       []any{actionButton(action)},
		})
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "flow",
		"horizontal_spacing": "small",
		"columns":            columns,
	}
}

func actionButton(action contracts.Action) map[string]any {
	return map[string]any{
		"tag":       "button",
		"type":      buttonType(action.Style),
		"size":      "small",
		"text":      map[string]any{"tag": "plain_text", "content": action.Label},
		"behaviors": []any{map[string]any{"type": "callback", "value": actionValue(action)}},
	}
}

func plainText(content, size, color string) map[string]any {
	return map[string]any{
		"tag": "div",
		"text": map[string]any{
			"tag":        "plain_text",
			"content":    content,
			"text_size":  size,
			"text_color": color,
		},
	}
}

func templateFor(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "green"
	case contracts.CardFailure, contracts.CardRoutingError:
		return "red"
	case contracts.CardRunningConflict:
		return "orange"
	case contracts.CardProjectSelection, contracts.CardThreadSelection:
		return "blue"
	default:
		if msg.Status == "failed" {
			return "red"
		}
		return "wathet"
	}
}

func cardSummary(msg contracts.OutboundMessage) string {
	if msg.Title != "" {
		return msg.Title
	}
	return statusLabel(msg)
}

func cardSubtitle(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "本机 Codex 已完成执行"
	case contracts.CardFailure:
		return "本机 Codex 需要你的处理"
	case contracts.CardProjectSelection:
		return "选择工作区后立即执行"
	case contracts.CardThreadSelection:
		return "从桌面 Codex 会话继续"
	case contracts.CardRunningConflict:
		return "当前任务仍在运行"
	case contracts.CardRoutingError:
		return "请从有效任务卡片继续"
	default:
		return "本机 Codex 远程任务"
	}
}

func statusTag(msg contracts.OutboundMessage) map[string]any {
	return map[string]any{
		"tag":   "text_tag",
		"text":  map[string]any{"tag": "plain_text", "content": statusLabel(msg)},
		"color": statusColor(msg),
	}
}

func statusLabel(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "已完成"
	case contracts.CardFailure:
		return "未完成"
	case contracts.CardRoutingError:
		return "待处理"
	case contracts.CardProjectSelection:
		return "选择项目"
	case contracts.CardThreadSelection:
		return "选择会话"
	case contracts.CardRunningConflict:
		return "运行中"
	}
	switch msg.Status {
	case "idle":
		return "已接管"
	case "queued":
		return "排队中"
	case "running":
		return "运行中"
	case "succeeded":
		return "已完成"
	case "failed":
		return "未完成"
	case "canceled":
		return "已停止"
	default:
		return "Codex"
	}
}

func statusColor(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "green"
	case contracts.CardFailure, contracts.CardRoutingError:
		return "red"
	case contracts.CardRunningConflict:
		return "orange"
	case contracts.CardProjectSelection, contracts.CardThreadSelection:
		return "blue"
	}
	switch msg.Status {
	case "idle", "succeeded":
		return "green"
	case "queued":
		return "orange"
	case "running":
		return "wathet"
	case "failed", "canceled":
		return "red"
	default:
		return "neutral"
	}
}

func actionValue(action contracts.Action) map[string]string {
	value := make(map[string]string, len(action.Value)+1)
	for key, val := range action.Value {
		value[key] = val
	}
	value["action_id"] = action.ID
	return value
}

func buttonType(style string) string {
	if style == "primary" {
		return "primary_filled"
	}
	if style == "" {
		return "default"
	}
	return style
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
