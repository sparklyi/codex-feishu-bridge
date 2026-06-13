package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sihuo/codex-feishu-bridge/internal/contracts"
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
			"title": map[string]any{"tag": "plain_text", "content": msg.Title},
		},
		"elements": []any{
			map[string]any{"tag": "markdown", "content": msg.BodyMarkdown},
		},
	}
	if len(msg.Actions) > 0 {
		elements := card["elements"].([]any)
		elements = append(elements,
			map[string]any{
				"tag":         "input",
				"name":        "text",
				"multiline":   true,
				"placeholder": map[string]any{"tag": "plain_text", "content": "Follow up"},
			},
			map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":  "button",
						"type": "primary",
						"text": map[string]any{"tag": "plain_text", "content": msg.Actions[0].Label},
						"value": map[string]any{
							"action_id": msg.Actions[0].ID,
						},
					},
				},
			},
		)
		card["elements"] = elements
	}
	return json.Marshal(card)
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
