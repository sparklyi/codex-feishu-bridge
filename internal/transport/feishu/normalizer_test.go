package feishu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestNormalizeMessageNewTask(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "/codex hello"}, "")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundNewTask || ev.Text != "/codex hello" || ev.DedupKey != "evt_1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.ChatType != "private" || ev.ChatID != "chat_1" || ev.SenderOpenID != "ou_owner" || ev.MessageID != "msg_1" || ev.RawReceivedAt.IsZero() {
		t.Fatalf("missing fields: %+v", ev)
	}
}

func TestNormalizeMessagePlainRootTextReachesRouter(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "hello"}, "")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundNewTask || ev.Text != "hello" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestNormalizeMessageReplyUsesRootMessageID(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "continue"}, "card_msg_1")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundReply || ev.RootMessageID != "card_msg_1" || ev.Text != "continue" {
		t.Fatalf("unexpected reply event: %+v", ev)
	}
}

func TestNormalizeMessageFallbackDedup(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "/codex @backend fix bug"}, "")
	raw = []byte(strings.Replace(string(raw), `"event_id":"evt_1",`, "", 1))
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.DedupKey != "message:msg_1" || ev.Text != "/codex @backend fix bug" {
		t.Fatalf("unexpected fallback dedup: %+v", ev)
	}
}

func TestNormalizeMessageBotMention(t *testing.T) {
	raw := messageJSONWithMentions(t, map[string]any{"text": "@_user_1 @backend fix tests"}, []string{"ou_bot"})
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify", BotOpenID: "ou_bot"})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.BotMentioned {
		t.Fatalf("bot mention not detected: %+v", ev)
	}
	if ev.Text != "@backend fix tests" {
		t.Fatalf("bot mention should be stripped, text=%q", ev.Text)
	}
}

func TestNormalizeMessageNonBotMentionIsNotStripped(t *testing.T) {
	raw := messageJSONWithMentions(t, map[string]any{"text": "@someone @backend fix tests"}, []string{"ou_other"})
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify", BotOpenID: "ou_bot"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.BotMentioned || ev.Text != "@someone @backend fix tests" {
		t.Fatalf("unexpected mention handling: %+v", ev)
	}
}

func TestNormalizeCardAction(t *testing.T) {
	raw := cardJSON(t, "continue", "token_1")
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundCardAction || ev.RootMessageID != "card_msg_1" || ev.ActionID != "continue_submit" || ev.Text != "continue" {
		t.Fatalf("unexpected card event: %+v", ev)
	}
	if ev.DedupKey != "token_1" {
		t.Fatalf("event/callback token should win, got %q", ev.DedupKey)
	}
}

func TestNormalizeCardFallbackDedupAndEmptyText(t *testing.T) {
	raw := cardJSON(t, "", "")
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "card:card_msg_1:ou_owner:continue_submit:"
	if !strings.HasPrefix(ev.DedupKey, wantPrefix) {
		t.Fatalf("dedup key = %q, want prefix %q", ev.DedupKey, wantPrefix)
	}
	if ev.Text != "" {
		t.Fatalf("empty callback text should normalize, got %+v", ev)
	}
}

func TestNormalizeRejectsWrongAppTokenAndMalformedPayload(t *testing.T) {
	if _, err := NormalizeMessageJSON(messageJSON(t, map[string]any{"text": "/codex hello"}, ""), VerifyOptions{AppID: "wrong", VerificationToken: "verify"}); err == nil {
		t.Fatal("expected wrong app id rejection")
	}
	if _, err := NormalizeMessageJSON(messageJSON(t, map[string]any{"text": "/codex hello"}, ""), VerifyOptions{AppID: "cli_test", VerificationToken: "wrong"}); err == nil {
		t.Fatal("expected wrong token rejection")
	}
	raw := cardJSON(t, "continue", "")
	raw = []byte(strings.Replace(string(raw), `"text":"continue"`, `"text":123`, 1))
	if _, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test", VerificationToken: "verify"}); err == nil {
		t.Fatal("expected malformed callback payload rejection")
	}
}

func messageJSON(t *testing.T, content map[string]any, root string) []byte {
	t.Helper()
	contentJSON, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	rootField := ""
	if root != "" {
		rootField = `,"parent_id":"` + root + `","root_id":"` + root + `"`
	}
	raw := `{
		"header":{"event_id":"evt_1","app_id":"cli_test","token":"verify","create_time":"1760000000000"},
		"event":{
			"sender":{"sender_id":{"open_id":"ou_owner"}},
			"message":{"message_id":"msg_1","chat_id":"chat_1","chat_type":"private","content":` + string(contentJSON) + rootField + `}
		}
	}`
	return []byte(raw)
}

func messageJSONWithMentions(t *testing.T, content map[string]any, openIDs []string) []byte {
	t.Helper()
	contentJSON, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := content["text"].(string)
	tokens := strings.Fields(text)
	mentions := make([]map[string]any, 0, len(openIDs))
	for i, openID := range openIDs {
		key := "@_user_1"
		if i < len(tokens) && strings.HasPrefix(tokens[i], "@") {
			key = tokens[i]
		}
		mentions = append(mentions, map[string]any{
			"key": key,
			"id":  map[string]any{"open_id": openID},
		})
	}
	mentionsJSON, err := json.Marshal(mentions)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{
		"header":{"event_id":"evt_1","app_id":"cli_test","token":"verify","create_time":"1760000000000"},
		"event":{
			"sender":{"sender_id":{"open_id":"ou_owner"}},
			"message":{"message_id":"msg_1","chat_id":"chat_1","chat_type":"group","content":` + string(contentJSON) + `,"mentions":` + string(mentionsJSON) + `}
		}
	}`
	return []byte(raw)
}

func cardJSON(t *testing.T, text, token string) []byte {
	t.Helper()
	tokenField := ""
	if token != "" {
		tokenField = `"event_id":"` + token + `",`
	}
	raw := `{
		"header":{` + tokenField + `"app_id":"cli_test","token":"verify","create_time":"1760000000000"},
		"event":{
			"operator":{"open_id":"ou_owner"},
			"context":{"open_message_id":"card_msg_1"},
			"message":{"message_id":"callback_msg_1","chat_id":"chat_1","chat_type":"private"},
			"action":{"action_id":"continue_submit","value":{"text":"` + text + `"}}
		}
	}`
	return []byte(raw)
}
