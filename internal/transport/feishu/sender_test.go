package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

func TestBuildInteractiveCardUsesCardJSONV2(t *testing.T) {
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
	decoded := decodeCard(t, card)
	if decoded["schema"] != "2.0" {
		t.Fatalf("card must use JSON 2.0: %s", string(card))
	}
	if _, legacy := decoded["elements"]; legacy {
		t.Fatalf("card must not retain the JSON 1.0 elements root: %s", string(card))
	}
	config, ok := decoded["config"].(map[string]any)
	if !ok || config["update_multi"] != true || config["width_mode"] != "fill" {
		t.Fatalf("card config must support shared patch updates: %s", string(card))
	}
	body, ok := decoded["body"].(map[string]any)
	if !ok {
		t.Fatalf("card must include a V2 body: %s", string(card))
	}
	markdown := taggedElements(body, "markdown")
	if len(markdown) != 1 || markdown[0]["element_id"] != "card_body" || markdown[0]["content"] != "done" {
		t.Fatalf("card body markdown malformed: %s", string(card))
	}
	if len(taggedElements(body, "form")) != 1 {
		t.Fatalf("continue action must render a V2 form: %s", string(card))
	}
}

func TestBuildInteractiveCardRendersStructuredOptionsWithCallbackBehaviors(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind:     contracts.CardProjectSelection,
		Title:        "Choose project",
		BodyMarkdown: "Select a project.",
		Options: []contracts.CardOption{
			{Title: "backend", Detail: "Fix the failing tests", Action: contracts.Action{ID: "project_select", Label: "选择", Style: "primary", Value: map[string]string{"action": "select_project", "project": "backend", "pending_id": "pi_1"}}},
			{Title: "frontend", Detail: "Polish the card", Action: contracts.Action{ID: "project_select", Label: "选择", Style: "primary", Value: map[string]string{"action": "select_project", "project": "frontend", "pending_id": "pi_1"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeCard(t, card)
	buttons := taggedElements(decoded["body"], "button")
	if len(buttons) != 2 || !jsonContains(string(card), "backend") || !jsonContains(string(card), "frontend") {
		t.Fatalf("structured options missing: %s", string(card))
	}
	projects := make(map[string]bool, len(buttons))
	for _, button := range buttons {
		if _, legacyValue := button["value"]; legacyValue {
			t.Fatalf("V2 buttons must use callback behaviors instead of value: %s", string(card))
		}
		if button["type"] != "primary_filled" {
			t.Fatalf("option button should use the primary V2 visual treatment: %s", string(card))
		}
		value := callbackValue(t, button, card)
		if value["action_id"] != "project_select" || value["action"] != "select_project" {
			t.Fatalf("option callback malformed: %s", string(card))
		}
		project, ok := value["project"].(string)
		if !ok {
			t.Fatalf("option callback missing project: %s", string(card))
		}
		projects[project] = true
	}
	if !projects["backend"] || !projects["frontend"] {
		t.Fatalf("option callbacks lost project values: %s", string(card))
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
		Actions: []contracts.Action{
			{ID: "continue_submit", Label: "继续跟进", Value: map[string]string{"action": "continue", "task_id": "cx_123"}},
			{ID: "stop_task", Label: "停止", Style: "danger", Value: map[string]string{"action": "stop_task", "task_id": "cx_123"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(card)
	for _, want := range []string{"状态", "已完成", "项目", "default", "工作区", "[local-path]", "继续补充需求或问题", "继续跟进"} {
		if !jsonContains(body, want) {
			t.Fatalf("card missing structured layout content %q: %s", want, body)
		}
	}
	if len(taggedElements(decodeCard(t, card)["body"], "column")) < 3 {
		t.Fatalf("task metadata must render as V2 columns: %s", body)
	}
	for _, banned := range []string{"任务信息", "wide_screen_mode", "\"action_type\":"} {
		if jsonContains(body, banned) {
			t.Fatalf("card retained a JSON 1.0 field %q: %s", banned, body)
		}
	}
}

func TestBuildInteractiveCardRendersContinueForm(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind:     contracts.CardSuccess,
		Title:        "任务已完成 · cx_123",
		BodyMarkdown: "**结果**\nHello",
		Actions: []contracts.Action{
			{ID: "continue_submit", Label: "继续跟进", Value: map[string]string{"action": "continue", "task_id": "cx_123"}},
			{ID: "stop_task", Label: "停止", Style: "danger", Value: map[string]string{"action": "stop_task", "task_id": "cx_123"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeCard(t, card)
	forms := taggedElements(decoded["body"], "form")
	if len(forms) != 1 {
		t.Fatalf("expected one V2 follow-up form: %s", string(card))
	}
	inputs := taggedElements(forms[0], "input")
	if len(inputs) != 1 {
		t.Fatalf("continue form input missing: %s", string(card))
	}
	followUpInput := inputs[0]
	var submitButton map[string]any
	for _, button := range taggedElements(forms[0], "button") {
		if button["name"] == "continue_submit" {
			submitButton = button
			break
		}
	}
	if followUpInput == nil || followUpInput["name"] != "text" || followUpInput["input_type"] != "multiline_text" || followUpInput["required"] != true {
		t.Fatalf("continue form input malformed: %s", string(card))
	}
	if followUpInput["max_length"] != float64(1000) {
		t.Fatalf("continue form max_length should not exceed Feishu default maximum: %s", string(card))
	}
	if submitButton == nil || submitButton["name"] != "continue_submit" || submitButton["form_action_type"] != "submit" {
		t.Fatalf("continue submit button malformed: %s", string(card))
	}
	submitValue := callbackValue(t, submitButton, card)
	if submitValue["action_id"] != "continue_submit" || submitValue["action"] != "continue" {
		t.Fatalf("continue submit value malformed: %s", string(card))
	}
	for _, button := range taggedElements(decoded["body"], "button") {
		value := callbackValue(t, button, card)
		if value["action_id"] == "stop_task" && button["type"] != "danger" {
			t.Fatalf("stop action should preserve danger styling: %s", string(card))
		}
	}
}

func TestBuildInteractiveCardAllowsPatchUpdates(t *testing.T) {
	card, err := BuildInteractiveCard(contracts.OutboundMessage{
		CardKind:     contracts.CardSuccess,
		Title:        "任务已完成 · cx_123",
		BodyMarkdown: "**结果**\nHello",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeCard(t, card)
	config, ok := decoded["config"].(map[string]any)
	if !ok || decoded["schema"] != "2.0" || config["update_multi"] != true {
		t.Fatalf("task cards must set update_multi for Feishu patch support: %s", string(card))
	}
	if _, enabled := config["streaming_mode"]; enabled {
		t.Fatalf("IM patch cards must not enable CardKit streaming mode: %s", string(card))
	}
}

func decodeCard(t *testing.T, card []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(card, &decoded); err != nil {
		t.Fatalf("invalid card json: %v\n%s", err, string(card))
	}
	return decoded
}

func taggedElements(value any, tag string) []map[string]any {
	var found []map[string]any
	var visit func(any)
	visit = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if typed["tag"] == tag {
				found = append(found, typed)
			}
			for _, child := range typed {
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return found
}

func callbackValue(t *testing.T, button map[string]any, card []byte) map[string]any {
	t.Helper()
	behaviors, ok := button["behaviors"].([]any)
	if !ok {
		t.Fatalf("button has no V2 callback behavior: %s", string(card))
	}
	for _, rawBehavior := range behaviors {
		behavior, ok := rawBehavior.(map[string]any)
		if !ok || behavior["type"] != "callback" {
			continue
		}
		value, ok := behavior["value"].(map[string]any)
		if !ok {
			t.Fatalf("callback behavior has no value: %s", string(card))
		}
		return value
	}
	t.Fatalf("button has no callback behavior: %s", string(card))
	return nil
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

func TestSenderRetriesTemporaryNetworkErrors(t *testing.T) {
	api := &fakeCardAPI{
		results: []sendResult{
			{err: temporarySendError{}},
			{messageID: "msg_1"},
		},
	}
	s := &Sender{API: api, MaxRetries: 1, Sleep: func(ctx context.Context, d time.Duration) error { return nil }}
	sent, err := s.Send(context.Background(), contracts.OutboundMessage{ChatID: "chat", CardKind: contracts.CardStart, TaskID: "cx", Title: "title", BodyMarkdown: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_1" || api.calls != 2 {
		t.Fatalf("unexpected send result: sent=%+v calls=%d", sent, api.calls)
	}
}

func TestSenderRetriesUnexpectedEOF(t *testing.T) {
	api := &fakeCardAPI{
		results: []sendResult{
			{err: io.ErrUnexpectedEOF},
			{messageID: "msg_1"},
		},
	}
	s := &Sender{API: api, MaxRetries: 1, Sleep: func(ctx context.Context, d time.Duration) error { return nil }}
	sent, err := s.Send(context.Background(), contracts.OutboundMessage{ChatID: "chat", CardKind: contracts.CardStart, TaskID: "cx", Title: "title", BodyMarkdown: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_1" || api.calls != 2 {
		t.Fatalf("unexpected send result: sent=%+v calls=%d", sent, api.calls)
	}
}

func TestRateLimitedResponsesAreTransient(t *testing.T) {
	err := feishuResponseError("patch", 99991400, "too frequent")
	if !errors.Is(err, ErrRateLimited) || !transport.IsTransientError(err) {
		t.Fatalf("rate-limited response must be transient: %v", err)
	}
	if isRateLimitedCode(12345) {
		t.Fatal("unexpected rate-limited code")
	}
}

func TestSenderPatchesUpdateTarget(t *testing.T) {
	api := &fakeCardAPI{}
	s := &Sender{API: api, MaxRetries: 1, Sleep: func(ctx context.Context, d time.Duration) error { return nil }}
	sent, err := s.Send(context.Background(), contracts.OutboundMessage{
		UpdateMessageID: "msg_original",
		CardKind:        contracts.CardStart,
		TaskID:          "cx",
		Title:           "title",
		BodyMarkdown:    "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_original" {
		t.Fatalf("patch should preserve updated message id, got %+v", sent)
	}
	if api.calls != 0 || api.patchCalls != 1 || api.lastPatchMessageID != "msg_original" {
		t.Fatalf("expected patch without create/reply, calls=%d patchCalls=%d lastPatch=%q", api.calls, api.patchCalls, api.lastPatchMessageID)
	}
}

func TestSenderUsesFreshAttemptContextsForPatchRetry(t *testing.T) {
	api := &contextTrackingCardAPI{}
	s := &Sender{
		API:            api,
		MaxRetries:     1,
		AttemptTimeout: time.Second,
		Sleep:          func(context.Context, time.Duration) error { return nil },
	}
	if _, err := s.Send(context.Background(), contracts.OutboundMessage{
		UpdateMessageID: "msg_original",
		CardKind:        contracts.CardStart,
		TaskID:          "cx",
		Title:           "title",
		BodyMarkdown:    "body",
	}); err != nil {
		t.Fatal(err)
	}
	if len(api.contexts) != 2 {
		t.Fatalf("patch attempts = %d, want 2", len(api.contexts))
	}
	if api.contexts[0] == api.contexts[1] {
		t.Fatal("patch retries must receive fresh contexts")
	}
	for _, attemptCtx := range api.contexts {
		if _, ok := attemptCtx.Deadline(); !ok {
			t.Fatal("patch attempt is missing a timeout")
		}
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
	results            []sendResult
	calls              int
	patchCalls         int
	lastPatchMessageID string
}

type sendResult struct {
	messageID  string
	retryAfter time.Duration
	err        error
}

type temporarySendError struct{}

func (temporarySendError) Error() string   { return "temporary timeout" }
func (temporarySendError) Timeout() bool   { return true }
func (temporarySendError) Temporary() bool { return true }

type contextTrackingCardAPI struct {
	contexts []context.Context
}

func (f *contextTrackingCardAPI) SendCard(context.Context, string, string, []byte) (string, time.Duration, error) {
	return "", 0, errors.New("unexpected send")
}

func (f *contextTrackingCardAPI) PatchCard(ctx context.Context, _ string, _ []byte) (time.Duration, error) {
	f.contexts = append(f.contexts, ctx)
	if len(f.contexts) == 1 {
		return 0, temporarySendError{}
	}
	return 0, nil
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

func (f *fakeCardAPI) PatchCard(ctx context.Context, messageID string, cardJSON []byte) (time.Duration, error) {
	f.patchCalls++
	f.lastPatchMessageID = messageID
	if len(f.results) == 0 {
		return 0, nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.retryAfter, result.err
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
