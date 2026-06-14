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
	if len(msg.Actions) != 1 || msg.Actions[0].ID != "continue_submit" {
		t.Fatalf("missing continue action: %+v", msg.Actions)
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
