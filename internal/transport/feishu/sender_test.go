package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestBuildInteractiveCard(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind:     contracts.CardSuccess,
		TaskID:       "cx_123",
		Status:       "succeeded",
		Title:        "Task cx_123",
		BodyMarkdown: "done",
		Actions:      []contracts.Action{{ID: "continue_submit", Label: "Continue"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(card, &decoded); err != nil {
		t.Fatalf("invalid card json: %v\n%s", err, string(card))
	}
	if string(card) == "" || !jsonContains(string(card), "continue_submit") || !jsonContains(string(card), "done") {
		t.Fatalf("card missing expected content: %s", string(card))
	}
}

func TestBuildInteractiveCardWithActionValues(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind:     contracts.CardProjectSelection,
		Title:        "Choose project",
		BodyMarkdown: "Select a project.",
		Fields:       []contracts.Field{{Title: "Prompt", Value: "fix tests"}},
		Actions: []contracts.Action{
			{ID: "project_select", Label: "backend", Value: map[string]string{"action": "select_project", "project": "backend", "pending_id": "pi_1"}},
			{ID: "project_select", Label: "frontend", Value: map[string]string{"action": "select_project", "project": "frontend", "pending_id": "pi_1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(card, &decoded); err != nil {
		t.Fatalf("invalid card json: %v", err)
	}
	if !jsonContains(string(card), "select_project") || !jsonContains(string(card), "backend") || !jsonContains(string(card), "Prompt") {
		t.Fatalf("card missing action values or fields: %s", string(card))
	}
	header := decoded["header"].(map[string]any)
	if header["template"] == nil {
		t.Fatalf("missing header template: %s", string(card))
	}
}

func TestBuildInteractiveCardUsesCompactTaskInfoSection(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind:     contracts.CardSuccess,
		Title:        "任务已完成 · cx_123",
		BodyMarkdown: "**结果**\nHello",
		Fields: []contracts.Field{
			{Title: "状态", Value: "已完成"},
			{Title: "项目", Value: "default"},
			{Title: "工作区", Value: "[local-path]"},
		},
		Actions: []contracts.Action{{ID: "continue_submit", Label: "继续跟进"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(card)
	for _, want := range []string{"任务信息", "状态：`已完成`", "项目：`default`", "继续补充需求或问题", "继续跟进"} {
		if !jsonContains(body, want) {
			t.Fatalf("card missing compact layout content %q: %s", want, body)
		}
	}
	for _, banned := range []string{"Status:", "Project:", "Workspace:", "Follow up"} {
		if jsonContains(body, banned) {
			t.Fatalf("card retained old layout text %q: %s", banned, body)
		}
	}
}

func TestSenderRateLimitRetryAndMessageID(t *testing.T) {
	api := &fakeCardAPI{
		results: []sendResult{
			{retryAfter: 5 * time.Millisecond, err: ErrRateLimited},
			{messageID: "msg_1"},
		},
	}
	s := &Sender{API: api, MaxRetries: 2, Sleep: func(ctx context.Context, d time.Duration) error { return nil }}
	sent, err := s.Send(context.Background(), contracts.OutboundMessage{ChatID: "chat", CardKind: contracts.CardStart, TaskID: "cx", Title: "title", BodyMarkdown: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_1" || api.calls != 2 {
		t.Fatalf("unexpected send result: sent=%+v calls=%d", sent, api.calls)
	}
}

func TestSenderRequiresMessageID(t *testing.T) {
	s := &Sender{API: &fakeCardAPI{results: []sendResult{{messageID: ""}}}}
	if _, err := s.Send(context.Background(), contracts.OutboundMessage{ChatID: "chat", CardKind: contracts.CardStart, TaskID: "cx", Title: "title"}); err == nil {
		t.Fatal("expected missing message id error")
	}
}

func TestNewSenderFromEnv(t *testing.T) {
	api := &fakeCardAPI{}
	s, err := NewSenderFromEnv("cli_test", "FEISHU_APP_SECRET", func(key string) string {
		if key == "FEISHU_APP_SECRET" {
			return "secret"
		}
		return ""
	}, api)
	if err != nil {
		t.Fatal(err)
	}
	if s.AppID != "cli_test" || s.AppSecret != "secret" || s.API != api {
		t.Fatalf("unexpected sender: %+v", s)
	}
	if _, err := NewSenderFromEnv("cli_test", "MISSING", func(string) string { return "" }, api); err == nil {
		t.Fatal("expected missing secret error")
	}
}

type fakeCardAPI struct {
	results []sendResult
	calls   int
}

type sendResult struct {
	messageID  string
	retryAfter time.Duration
	err        error
}

func (f *fakeCardAPI) SendCard(ctx context.Context, chatID, replyToMessageID string, cardJSON []byte) (string, time.Duration, error) {
	f.calls++
	if len(f.results) == 0 {
		return "", 0, errors.New("no result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.messageID, result.retryAfter, result.err
}

func jsonContains(s, needle string) bool {
	return len(s) >= len(needle) && (s == needle || contains(s, needle))
}

func contains(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
