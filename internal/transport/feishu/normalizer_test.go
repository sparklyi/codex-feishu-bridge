package feishu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestNormalizeMessageNewTask(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "review current changes"}, "")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundNewTask || ev.Text != "review current changes" || ev.DedupKey != "evt_1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.ChatType != "private" || ev.ChatID != "chat_1" || ev.SenderOpenID != "ou_owner" || ev.MessageID != "msg_1" || ev.RawReceivedAt.IsZero() {
		t.Fatalf("missing fields: %+v", ev)
	}
}

func TestNormalizeMessagePlainRootTextReachesRouter(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "hello"}, "")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundNewTask || ev.Text != "hello" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestNormalizeMessageReplyUsesRootMessageID(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "continue"}, "card_msg_1")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundReply || ev.RootMessageID != "card_msg_1" || ev.Text != "continue" {
		t.Fatalf("unexpected reply event: %+v", ev)
	}
}

func TestNormalizeMessageFallbackDedup(t *testing.T) {
	raw := messageJSON(t, map[string]any{"text": "@backend fix bug"}, "")
	raw = []byte(strings.Replace(string(raw), `"event_id":"evt_1",`, "", 1))
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.DedupKey != "message:msg_1" || ev.Text != "@backend fix bug" {
		t.Fatalf("unexpected fallback dedup: %+v", ev)
	}
}

func TestNormalizeNonPrivateChat(t *testing.T) {
	raw := messageJSONWithChatType(t, map[string]any{"text": "fix tests"}, "", "group")
	ev, err := NormalizeMessageJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ChatType != "non_private" {
		t.Fatalf("chat type = %q, want non_private", ev.ChatType)
	}
}

func TestNormalizeCardAction(t *testing.T) {
	raw := cardJSON(t, "continue", "token_1")
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != contracts.InboundCardAction || ev.RootMessageID != "card_msg_1" || ev.ActionID != "continue_submit" || ev.Text != "continue" {
		t.Fatalf("unexpected card event: %+v", ev)
	}
	if ev.DedupKey != "token_1" {
		t.Fatalf("event id should win, got %q", ev.DedupKey)
	}
}

func TestNormalizeCardFallbackDedupAndEmptyText(t *testing.T) {
	raw := cardJSON(t, "", "")
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test"})
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

func TestNormalizeCardActionValues(t *testing.T) {
	raw := cardJSONWithValue(t, map[string]any{
		"action":  "stop_task",
		"task_id": "cx_1",
	}, "token_1")
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ActionValue["action"] != "stop_task" || ev.ActionValue["task_id"] != "cx_1" || ev.Text != "" {
		t.Fatalf("unexpected callback values: %+v", ev)
	}
}

func TestNormalizeRejectsWrongAppAndMalformedPayload(t *testing.T) {
	if _, err := NormalizeMessageJSON(messageJSON(t, map[string]any{"text": "review current changes"}, ""), VerifyOptions{AppID: "wrong"}); err == nil {
		t.Fatal("expected wrong app id rejection")
	}
	raw := cardJSON(t, "continue", "")
	raw = []byte(strings.Replace(string(raw), `"text":"continue"`, `"text":123`, 1))
	if _, err := NormalizeCardActionJSON(raw, VerifyOptions{AppID: "cli_test"}); err == nil {
		t.Fatal("expected malformed callback payload rejection")
	}
}

func messageJSON(t *testing.T, content map[string]any, root string) []byte {
	return messageJSONWithChatType(t, content, root, "private")
}

func messageJSONWithChatType(t *testing.T, content map[string]any, root, chatType string) []byte {
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
		"header":{"event_id":"evt_1","app_id":"cli_test","create_time":"1760000000000"},
		"event":{
			"sender":{"sender_id":{"open_id":"ou_owner"}},
			"message":{"message_id":"msg_1","chat_id":"chat_1","chat_type":"` + chatType + `","content":` + string(contentJSON) + rootField + `}
		}
	}`
	return []byte(raw)
}

func cardJSONWithValue(t *testing.T, value map[string]any, token string) []byte {
	t.Helper()
	valueJSON, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	tokenField := ""
	if token != "" {
		tokenField = `"event_id":"` + token + `",`
	}
	raw := `{
		"header":{` + tokenField + `"app_id":"cli_test","create_time":"1760000000000"},
		"event":{
			"operator":{"open_id":"ou_owner"},
			"context":{"open_message_id":"card_msg_1"},
			"message":{"message_id":"callback_msg_1","chat_id":"chat_1","chat_type":"private"},
			"action":{"action_id":"continue_submit","value":` + string(valueJSON) + `}
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
		"header":{` + tokenField + `"app_id":"cli_test","create_time":"1760000000000"},
		"event":{
			"operator":{"open_id":"ou_owner"},
			"context":{"open_message_id":"card_msg_1"},
			"message":{"message_id":"callback_msg_1","chat_id":"chat_1","chat_type":"private"},
			"action":{"action_id":"continue_submit","value":{"text":"` + text + `"}}
		}
	}`
	return []byte(raw)
}
