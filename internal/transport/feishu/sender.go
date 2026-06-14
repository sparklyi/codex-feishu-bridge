package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

var ErrRateLimited = errors.New("feishu rate limited")

type CardAPI interface {
	SendCard(ctx context.Context, chatID, replyToMessageID string, cardJSON []byte) (messageID string, retryAfter time.Duration, err error)
}

type Sender struct {
	AppID      string
	AppSecret  string
	API        CardAPI
	MaxRetries int
	Sleep      func(context.Context, time.Duration) error
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

func NewSDKCardAPI(appID, appSecret string) *SDKCardAPI {
	return &SDKCardAPI{client: lark.NewClient(appID, appSecret)}
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
			return "", 0, fmt.Errorf("feishu reply failed: code=%d msg=%s", resp.Code, resp.Msg)
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
		return "", 0, fmt.Errorf("feishu send failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", 0, nil
	}
	return *resp.Data.MessageId, 0, nil
}

func (s *Sender) Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error) {
	if s.API == nil {
		return contracts.SentMessage{}, errors.New("feishu sender API is nil")
	}
	card, err := BuildInteractiveCard(msg)
	if err != nil {
		return contracts.SentMessage{}, err
	}
	maxRetries := s.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		messageID, retryAfter, err := s.API.SendCard(ctx, msg.ChatID, msg.ReplyToMessageID, card)
		if err == nil {
			if messageID == "" {
				return contracts.SentMessage{}, errors.New("Feishu send returned empty message id")
			}
			return contracts.SentMessage{MessageID: messageID}, nil
		}
		lastErr = err
		if !errors.Is(err, ErrRateLimited) || attempt == maxRetries {
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

func BuildInteractiveCard(msg contracts.OutboundMessage) ([]byte, error) {
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": templateFor(msg),
			"title":    map[string]any{"tag": "plain_text", "content": msg.Title},
		},
		"elements": []any{
			map[string]any{"tag": "markdown", "content": msg.BodyMarkdown},
		},
	}
	if len(msg.Fields) > 0 {
		elements := card["elements"].([]any)
		elements = append(elements,
			map[string]any{"tag": "hr"},
			map[string]any{"tag": "markdown", "content": fieldMarkdown(msg.Fields)},
		)
		card["elements"] = elements
	}
	if len(msg.Actions) > 0 {
		elements := card["elements"].([]any)
		var followUpAction *contracts.Action
		buttonActions := make([]contracts.Action, 0, len(msg.Actions))
		for _, action := range msg.Actions {
			if action.ID == "continue_submit" {
				actionCopy := action
				followUpAction = &actionCopy
				continue
			}
			if isTaskCard(msg.CardKind) {
				continue
			}
			buttonActions = append(buttonActions, action)
		}
		if len(buttonActions) > 0 {
			actions := make([]any, 0, len(buttonActions))
			for _, action := range buttonActions {
				actions = append(actions, map[string]any{
					"tag":   "button",
					"type":  buttonType(action.Style),
					"text":  map[string]any{"tag": "plain_text", "content": action.Label},
					"value": actionValue(action),
				})
			}
			elements = append(elements, map[string]any{"tag": "action", "layout": "flow", "actions": actions})
		}
		if followUpAction != nil {
			elements = append(elements, followUpForm(*followUpAction))
		}
		card["elements"] = elements
	}
	return json.Marshal(card)
}

func isTaskCard(kind contracts.CardKind) bool {
	return kind == contracts.CardStart || kind == contracts.CardSuccess || kind == contracts.CardFailure
}

func followUpForm(action contracts.Action) map[string]any {
	return map[string]any{
		"tag":  "form",
		"name": "codex_followup_form",
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
				"fallback": map[string]any{
					"tag": "fallback_text",
					"text": map[string]any{
						"tag":     "plain_text",
						"content": "当前飞书客户端不支持卡片输入框，请直接回复任务卡片。",
					},
				},
			},
			map[string]any{
				"tag":         "button",
				"name":        action.ID,
				"action_type": "form_submit",
				"type":        buttonType(action.Style),
				"text":        map[string]any{"tag": "plain_text", "content": action.Label},
				"value":       actionValue(action),
			},
		},
		"fallback": map[string]any{
			"tag": "fallback_text",
			"text": map[string]any{
				"tag":     "plain_text",
				"content": "当前飞书客户端不支持卡片表单，请直接回复任务卡片。",
			},
		},
	}
}

func templateFor(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "green"
	case contracts.CardFailure, contracts.CardRoutingError:
		return "red"
	case contracts.CardRunningConflict, contracts.CardShortcutConfirm:
		return "orange"
	case contracts.CardProjectSelection, contracts.CardMigrationHint:
		return "blue"
	default:
		if msg.Status == "failed" {
			return "red"
		}
		return "wathet"
	}
}

func fieldMarkdown(fields []contracts.Field) string {
	lines := make([]string, 0, len(fields)+1)
	lines = append(lines, "**任务信息**")
	for _, field := range fields {
		lines = append(lines, field.Title+"：`"+field.Value+"`")
	}
	return strings.Join(lines, "\n")
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
