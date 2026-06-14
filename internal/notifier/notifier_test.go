package notifier

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	for _, want := range []string{"cx_123", "running", "backend", "[local-path]"} {
		if !strings.Contains(msg.BodyMarkdown, want) {
			t.Fatalf("card body missing %q: %q", want, msg.BodyMarkdown)
		}
	}
	for _, banned := range []string{"/Users/sihuo", "abc123", "user:pass@", "019ec257-e6fd-7be1-9a5e-c47442df292c"} {
		if strings.Contains(msg.BodyMarkdown, banned) || strings.Contains(msg.Title, banned) {
			t.Fatalf("card leaked %q in %+v", banned, msg)
		}
	}
	if len(msg.Actions) < 5 || msg.Actions[0].ID != "continue_submit" {
		t.Fatalf("missing continue action: %+v", msg.Actions)
	}
	if msg.Actions[1].Value["shortcut"] != "summarize" {
		t.Fatalf("missing shortcut action payloads: %+v", msg.Actions)
	}
	if len(msg.Fields) == 0 {
		t.Fatalf("missing compact metadata fields: %+v", msg)
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
