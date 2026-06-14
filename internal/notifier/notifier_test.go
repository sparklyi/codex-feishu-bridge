package notifier

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestTaskCardsAreRedactedAndRouteable(t *testing.T) {
	sender := &fakeSender{messageID: "msg_start"}
	n := New(sender)
	sent, err := n.Start(context.Background(), TaskCardInput{
		ChatID:         "chat_1",
		TaskID:         "cx_123",
		Status:         "running",
		ProjectAlias:   "backend",
		CWDLabel:       "/Users/sihuo/private/backend",
		Body:           "token=abc123 http://user:pass@proxy.local:7890",
		CodexSessionID: "019ec257-e6fd-7be1-9a5e-c47442df292c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_start" {
		t.Fatalf("unexpected sent message: %+v", sent)
	}
	msg := sender.messages[0]
	if !strings.Contains(msg.Title, "cx_123") {
		t.Fatalf("card title missing task id: %q", msg.Title)
	}
	if !strings.Contains(msg.BodyMarkdown, "Codex 正在处理") {
		t.Fatalf("card body missing running progress: %q", msg.BodyMarkdown)
	}
	gotFields := map[string]string{}
	for _, field := range msg.Fields {
		gotFields[field.Title] = field.Value
	}
	for title, want := range map[string]string{"状态": "运行中", "项目": "backend", "工作区": "[local-path]"} {
		if gotFields[title] != want {
			t.Fatalf("field %q mismatch: got %q want %q in %+v", title, gotFields[title], want, msg.Fields)
		}
	}
	for _, banned := range []string{"/Users/sihuo", "abc123", "user:pass@", "019ec257-e6fd-7be1-9a5e-c47442df292c"} {
		if strings.Contains(msg.BodyMarkdown, banned) || strings.Contains(msg.Title, banned) {
			t.Fatalf("card leaked %q in %+v", banned, msg)
		}
	}
	if len(msg.Actions) != 1 || msg.Actions[0].ID != "continue_submit" || msg.Actions[0].Label != "继续跟进" {
		t.Fatalf("task card should only expose the continue action: %+v", msg.Actions)
	}
	if len(msg.Fields) == 0 {
		t.Fatalf("missing compact metadata fields: %+v", msg)
	}
}

func TestTaskCardsUseCompactChineseLayout(t *testing.T) {
	sender := &fakeSender{messageID: "msg_success"}
	n := New(sender)
	_, err := n.Success(context.Background(), TaskCardInput{
		ChatID:       "chat_1",
		TaskID:       "cx_8ae94f",
		Status:       "succeeded",
		ProjectAlias: "default",
		CWDLabel:     "/Users/sihuo/GoProject/codex-feishu-bridge",
		Body:         "Hello，我在。",
	})
	if err != nil {
		t.Fatal(err)
	}

	msg := sender.messages[0]
	if msg.Title != "任务已完成 · cx_8ae94f" {
		t.Fatalf("unexpected localized title: %q", msg.Title)
	}
	for _, banned := range []string{"Task:", "Status:", "Project:", "Workspace:"} {
		if strings.Contains(msg.BodyMarkdown, banned) {
			t.Fatalf("task metadata leaked into body as raw label %q: %q", banned, msg.BodyMarkdown)
		}
	}
	if !strings.Contains(msg.BodyMarkdown, "Hello，我在。") || !strings.Contains(msg.BodyMarkdown, "**结果**") {
		t.Fatalf("body should focus on the codex result: %q", msg.BodyMarkdown)
	}
	if !strings.Contains(msg.BodyMarkdown, "直接回复这张任务卡片") {
		t.Fatalf("body should include reply fallback for continuing the task: %q", msg.BodyMarkdown)
	}
	wantFields := []contracts.Field{
		{Title: "状态", Value: "已完成"},
		{Title: "项目", Value: "default"},
		{Title: "工作区", Value: "[local-path]"},
	}
	if len(msg.Fields) != len(wantFields) {
		t.Fatalf("unexpected fields: %+v", msg.Fields)
	}
	for i, want := range wantFields {
		if msg.Fields[i] != want {
			t.Fatalf("field %d mismatch: got %+v want %+v", i, msg.Fields[i], want)
		}
	}
	if len(msg.Actions) != 1 || msg.Actions[0].ID != "continue_submit" || msg.Actions[0].Label != "继续跟进" {
		t.Fatalf("task card should only expose the continue action: %+v", msg.Actions)
	}
}

func TestFollowUpAcceptedUpdatesOriginalCardWithoutForm(t *testing.T) {
	sender := &fakeSender{messageID: "msg_result"}
	n := New(sender)
	sent, err := n.FollowUpAccepted(context.Background(), FollowUpAcceptedInput{
		ChatID:          "chat_1",
		UpdateMessageID: "msg_result",
		TaskID:          "cx_8ae94f",
		ProjectAlias:    "default",
		CWDLabel:        "/Users/sihuo/GoProject/codex-feishu-bridge",
		Prompt:          "测试继续处理并检查状态",
		SubmittedAt:     time.Date(2026, 6, 14, 20, 55, 0, 0, time.FixedZone("CST", 8*60*60)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_result" {
		t.Fatalf("updated card should preserve original message id: %+v", sent)
	}
	msg := sender.messages[0]
	if msg.UpdateMessageID != "msg_result" {
		t.Fatalf("expected original card update target: %+v", msg)
	}
	if msg.Title != "继续处理中 · cx_8ae94f" || msg.Status != "running" {
		t.Fatalf("unexpected follow-up accepted title/status: %+v", msg)
	}
	for _, want := range []string{"已收到继续跟进", "2026-06-14 20:55:00 CST", "测试继续处理并检查状态"} {
		if !strings.Contains(msg.BodyMarkdown, want) {
			t.Fatalf("accepted card missing %q: %q", want, msg.BodyMarkdown)
		}
	}
	if len(msg.Actions) != 0 {
		t.Fatalf("running accepted card must not expose another follow-up form: %+v", msg.Actions)
	}
}

func TestFollowUpAcceptedCanSendFallbackAcknowledgement(t *testing.T) {
	sender := &fakeSender{messageID: "msg_ack"}
	n := New(sender)
	sent, err := n.FollowUpAccepted(context.Background(), FollowUpAcceptedInput{
		ChatID:      "chat_1",
		TaskID:      "cx_8ae94f",
		Status:      "running",
		Prompt:      "fallback ack",
		SubmittedAt: time.Date(2026, 6, 14, 20, 55, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "msg_ack" {
		t.Fatalf("unexpected fallback ack message id: %+v", sent)
	}
	msg := sender.messages[0]
	if msg.UpdateMessageID != "" || msg.CardKind != contracts.CardStart || len(msg.Actions) != 0 {
		t.Fatalf("fallback acknowledgement should be a new no-form running card: %+v", msg)
	}
}

func TestProjectSelectionCardActions(t *testing.T) {
	sender := &fakeSender{messageID: "msg_project"}
	n := New(sender)
	_, err := n.ProjectSelection(context.Background(), ProjectSelectionInput{
		ChatID: "chat", ReplyToMessageID: "msg_user", PendingID: "pi_1",
		Prompt: "fix tests", ProjectAliases: []string{"backend", "frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := sender.messages[0]
	if msg.CardKind != contracts.CardProjectSelection || len(msg.Actions) != 2 {
		t.Fatalf("unexpected project selection: %+v", msg)
	}
	if msg.Actions[0].Value["action"] != "select_project" || msg.Actions[0].Value["pending_id"] != "pi_1" {
		t.Fatalf("missing action payload: %+v", msg.Actions[0])
	}
}

func TestRunningConflictAndMigrationCards(t *testing.T) {
	sender := &fakeSender{messageID: "msg_status"}
	n := New(sender)
	if err := n.RunningConflict(context.Background(), RunningConflictInput{
		ChatID: "chat", ReplyToMessageID: "msg_user", TaskID: "cx_1", Status: "running", ProjectAlias: "backend",
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.MigrationHint(context.Background(), "chat", "msg_old"); err != nil {
		t.Fatal(err)
	}
	if sender.messages[0].CardKind != contracts.CardRunningConflict || sender.messages[0].TaskID != "cx_1" {
		t.Fatalf("unexpected running conflict card: %+v", sender.messages[0])
	}
	if sender.messages[1].CardKind != contracts.CardMigrationHint {
		t.Fatalf("unexpected migration hint card: %+v", sender.messages[1])
	}
}

func TestShortcutConfirmationActions(t *testing.T) {
	sender := &fakeSender{messageID: "msg_confirm"}
	n := New(sender)
	if _, err := n.ShortcutConfirmation(context.Background(), ShortcutConfirmationInput{
		ChatID: "chat", ReplyToMessageID: "msg_user", RootMessageID: "msg_result",
		Shortcut: "run_tests", Prompt: "Run tests.",
	}); err != nil {
		t.Fatal(err)
	}
	msg := sender.messages[0]
	if msg.CardKind != contracts.CardShortcutConfirm || len(msg.Actions) != 2 {
		t.Fatalf("unexpected confirmation card: %+v", msg)
	}
	if msg.Actions[0].Value["action"] != "confirm_shortcut" || msg.Actions[1].Value["action"] != "cancel_shortcut" {
		t.Fatalf("missing confirmation payloads: %+v", msg.Actions)
	}
}

func TestRoutingErrorCardHasNoTaskID(t *testing.T) {
	sender := &fakeSender{messageID: "msg_err"}
	n := New(sender)
	if _, err := n.RoutingError(context.Background(), "chat_1", "reply_to"); err != nil {
		t.Fatal(err)
	}
	msg := sender.messages[0]
	if msg.TaskID != "" || msg.CardKind != contracts.CardRoutingError {
		t.Fatalf("unexpected routing error card: %+v", msg)
	}
	if !strings.Contains(msg.BodyMarkdown, "reply from a task card") {
		t.Fatalf("unexpected routing error body: %q", msg.BodyMarkdown)
	}
}

func TestCardTruncationAndMissingMessageID(t *testing.T) {
	sender := &fakeSender{messageID: "msg_ok"}
	n := New(sender)
	if _, err := n.Success(context.Background(), TaskCardInput{ChatID: "chat", TaskID: "cx", Status: "succeeded", Body: strings.Repeat("x", 5000)}); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages[0].BodyMarkdown) > 4000 {
		t.Fatalf("success body too long: %d", len(sender.messages[0].BodyMarkdown))
	}
	sender.messages = nil
	if _, err := n.Failure(context.Background(), TaskCardInput{ChatID: "chat", TaskID: "cx", Status: "failed", Body: strings.Repeat("x", 5000)}); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages[0].BodyMarkdown) > 2000 {
		t.Fatalf("failure body too long: %d", len(sender.messages[0].BodyMarkdown))
	}
	sender.messageID = ""
	if _, err := n.Start(context.Background(), TaskCardInput{ChatID: "chat", TaskID: "cx"}); err == nil {
		t.Fatal("expected missing message id error")
	}
}

type fakeSender struct {
	messageID string
	err       error
	messages  []contracts.OutboundMessage
}

func (f *fakeSender) Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error) {
	f.messages = append(f.messages, msg)
	if f.err != nil {
		return contracts.SentMessage{}, f.err
	}
	return contracts.SentMessage{MessageID: f.messageID}, nil
}

func TestNotifierReturnsSenderError(t *testing.T) {
	want := errors.New("send failed")
	n := New(&fakeSender{err: want})
	if _, err := n.Success(context.Background(), TaskCardInput{ChatID: "chat", TaskID: "cx"}); !errors.Is(err, want) {
		t.Fatalf("expected sender error, got %v", err)
	}
}
