package feishu

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

type VerifyOptions struct {
	AppID             string
	VerificationToken string
	BotOpenID         string
}

func NormalizeMessageJSON(raw []byte, opts VerifyOptions) (contracts.InboundEvent, error) {
	var msg messageEnvelope
	if err := json.Unmarshal(raw, &msg); err != nil {
		return contracts.InboundEvent{}, err
	}
	if err := verifyHeader(msg.Header, opts); err != nil {
		return contracts.InboundEvent{}, err
	}
	text, err := extractMessageText(msg.Event.Message.Content)
	if err != nil {
		return contracts.InboundEvent{}, err
	}
	text, botMentioned := normalizeBotMention(text, msg.Event.Message.Mentions, opts.BotOpenID)
	rootID := msg.Event.Message.RootID
	if rootID == "" {
		rootID = msg.Event.Message.ParentID
	}
	kind := contracts.InboundReply
	if rootID == "" {
		kind = contracts.InboundNewTask
	}
	dedup := msg.Header.EventID
	if dedup == "" {
		dedup = "message:" + msg.Event.Message.MessageID
	}
	return contracts.InboundEvent{
		DedupKey:      dedup,
		Kind:          kind,
		ChatType:      normalizeChatType(msg.Event.Message.ChatType),
		ChatID:        msg.Event.Message.ChatID,
		SenderOpenID:  msg.Event.Sender.SenderID.OpenID,
		MessageID:     msg.Event.Message.MessageID,
		RootMessageID: rootID,
		BotMentioned:  botMentioned,
		Text:          text,
		RawReceivedAt: parseFeishuTime(msg.Header.CreateTime),
	}, nil
}

func NormalizeCardActionJSON(raw []byte, opts VerifyOptions) (contracts.InboundEvent, error) {
	var card cardEnvelope
	if err := json.Unmarshal(raw, &card); err != nil {
		return contracts.InboundEvent{}, err
	}
	if err := verifyHeader(card.Header, opts); err != nil {
		return contracts.InboundEvent{}, err
	}
	actionValue, textRaw, err := normalizeActionValue(card.Event.Action.Value)
	if err != nil {
		return contracts.InboundEvent{}, err
	}
	text := actionValue["text"]
	rootID := card.Event.Context.OpenMessageID
	dedup := card.Header.EventID
	if dedup == "" {
		sum := sha256.Sum256(textRaw)
		dedup = "card:" + rootID + ":" + card.Event.Operator.OpenID + ":" + card.Event.Action.ActionID + ":" + hex.EncodeToString(sum[:])[:12]
	}
	return contracts.InboundEvent{
		DedupKey:      dedup,
		Kind:          contracts.InboundCardAction,
		ChatType:      normalizeChatType(card.Event.Message.ChatType),
		ChatID:        card.Event.Message.ChatID,
		SenderOpenID:  card.Event.Operator.OpenID,
		MessageID:     card.Event.Message.MessageID,
		RootMessageID: rootID,
		ActionID:      card.Event.Action.ActionID,
		ActionValue:   actionValue,
		Text:          text,
		RawReceivedAt: parseFeishuTime(card.Header.CreateTime),
	}, nil
}

func verifyHeader(header feishuHeader, opts VerifyOptions) error {
	if opts.AppID != "" && header.AppID != opts.AppID {
		return fmt.Errorf("unexpected app id")
	}
	if opts.VerificationToken != "" && header.Token != opts.VerificationToken {
		return fmt.Errorf("invalid verification token")
	}
	return nil
}

func extractMessageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var contentBytes []byte
	if raw[0] == '"' {
		var contentString string
		if err := json.Unmarshal(raw, &contentString); err != nil {
			return "", err
		}
		contentBytes = []byte(contentString)
	} else {
		contentBytes = raw
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(contentBytes, &content); err != nil {
		return "", err
	}
	return content.Text, nil
}

func normalizeActionValue(values map[string]json.RawMessage) (map[string]string, json.RawMessage, error) {
	actionValue := make(map[string]string, len(values))
	textRaw, ok := values["text"]
	if !ok {
		textRaw = json.RawMessage(`""`)
	}
	for key, raw := range values {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			if key == "text" {
				return nil, nil, fmt.Errorf("callback text must be a string: %w", err)
			}
			continue
		}
		actionValue[key] = value
	}
	if _, ok := actionValue["text"]; !ok {
		actionValue["text"] = ""
	}
	return actionValue, textRaw, nil
}

func normalizeBotMention(text string, mentions []messageMention, botOpenID string) (string, bool) {
	if botOpenID == "" {
		return text, false
	}
	for _, mention := range mentions {
		if mention.ID.OpenID != botOpenID {
			continue
		}
		stripped, ok := stripLeadingMention(text, mention.Key)
		if ok {
			text = stripped
		}
		return text, true
	}
	return text, false
}

func stripLeadingMention(text, key string) (string, bool) {
	text = strings.TrimSpace(text)
	key = strings.TrimSpace(key)
	if key == "" {
		return text, false
	}
	candidates := []string{key}
	if !strings.HasPrefix(key, "@") {
		candidates = append(candidates, "@"+key)
	}
	for _, candidate := range candidates {
		if text == candidate {
			return "", true
		}
		if !strings.HasPrefix(text, candidate) {
			continue
		}
		rest := text[len(candidate):]
		if rest == "" {
			return "", true
		}
		r, _ := utf8.DecodeRuneInString(rest)
		if unicode.IsSpace(r) {
			return strings.TrimSpace(rest), true
		}
	}
	return text, false
}

func parseFeishuTime(value string) time.Time {
	if value == "" {
		return time.Now().UTC()
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.UnixMilli(ms).UTC()
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func normalizeChatType(value string) string {
	switch value {
	case "private", "p2p":
		return "private"
	case "group", "topic_group":
		return "group"
	case "":
		return "unknown"
	default:
		return value
	}
}

type feishuHeader struct {
	EventID    string `json:"event_id"`
	AppID      string `json:"app_id"`
	Token      string `json:"token"`
	CreateTime string `json:"create_time"`
}

type messageEnvelope struct {
	Header feishuHeader `json:"header"`
	Event  struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		Message struct {
			MessageID string           `json:"message_id"`
			ChatID    string           `json:"chat_id"`
			ChatType  string           `json:"chat_type"`
			Content   json.RawMessage  `json:"content"`
			Mentions  []messageMention `json:"mentions"`
			ParentID  string           `json:"parent_id"`
			RootID    string           `json:"root_id"`
		} `json:"message"`
	} `json:"event"`
}

type messageMention struct {
	Key string `json:"key"`
	ID  struct {
		OpenID string `json:"open_id"`
	} `json:"id"`
}

type cardEnvelope struct {
	Header feishuHeader `json:"header"`
	Event  struct {
		Operator struct {
			OpenID string `json:"open_id"`
		} `json:"operator"`
		Context struct {
			OpenMessageID string `json:"open_message_id"`
		} `json:"context"`
		Message struct {
			MessageID string `json:"message_id"`
			ChatID    string `json:"chat_id"`
			ChatType  string `json:"chat_type"`
		} `json:"message"`
		Action struct {
			ActionID string                     `json:"action_id"`
			Value    map[string]json.RawMessage `json:"value"`
		} `json:"action"`
	} `json:"event"`
}
