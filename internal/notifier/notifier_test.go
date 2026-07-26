package notifier

import (
	"context"
	"strings"
	"testing"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestTaskCardsStreamAndExposeStopOnlyWhileActive(t *testing.T) {
	sender := &fakeSender{ids: []string{"message-1", "message-1", "message-1"}}
	n := New(sender)
	start, err := n.Start(context.Background(), TaskCardInput{
		ChatID: "chat", TaskID: "task-1", Status: "queued", ProjectAlias: "backend", CWDLabel: "/Users/alice/repo", Body: "starting",
	})
	if err != nil || start.MessageID != "message-1" {
		t.Fatalf("start result=%+v err=%v", start, err)
	}
	if len(sender.messages) != 1 || sender.messages[0].CardKind != contracts.CardStart || !hasAction(sender.messages[0], "continue_submit") || !hasAction(sender.messages[0], "stop_task") {
		t.Fatalf("unexpected start card: %+v", sender.messages)
	}
	if strings.Contains(sender.messages[0].BodyMarkdown, "/Users/alice/repo") {
		t.Fatalf("absolute paths should be redacted: %q", sender.messages[0].BodyMarkdown)
	}
	if _, err := n.Progress(context.Background(), TaskCardInput{ChatID: "chat", UpdateMessageID: "message-1", TaskID: "task-1", Status: "running", Body: "working"}); err != nil {
		t.Fatal(err)
	}
	if sender.messages[1].UpdateMessageID != "message-1" || sender.messages[1].Status != "running" {
		t.Fatalf("progress must patch running card: %+v", sender.messages[1])
	}
	if _, err := n.Success(context.Background(), TaskCardInput{ChatID: "chat", UpdateMessageID: "message-1", TaskID: "task-1", Status: "succeeded", Body: "done"}); err != nil {
		t.Fatal(err)
	}
	if hasAction(sender.messages[2], "stop_task") || !hasAction(sender.messages[2], "continue_submit") {
		t.Fatalf("terminal card actions incorrect: %+v", sender.messages[2].Actions)
	}
}

func TestThreadSelectionCard(t *testing.T) {
	sender := &fakeSender{ids: []string{"threads"}}
	n := New(sender)
	if _, err := n.ThreadSelection(context.Background(), ThreadSelectionInput{
		ChatID: "chat", ReplyToMessageID: "input", Threads: []ThreadOption{{ID: "thread-secret", Name: "Desktop work", Preview: "Fix the failing test", CWD: "/repo"}},
	}); err != nil {
		t.Fatal(err)
	}
	threadCard := sender.messages[0]
	if threadCard.CardKind != contracts.CardThreadSelection || !hasOptionAction(threadCard, "attach_thread") || strings.Contains(threadCard.BodyMarkdown, "thread-secret") {
		t.Fatalf("unexpected thread card: %+v", threadCard)
	}
}

func TestRejectionCard(t *testing.T) {
	sender := &fakeSender{ids: []string{"error"}}
	n := New(sender)
	if err := n.Rejection(context.Background(), "chat", "input", "bad request"); err != nil {
		t.Fatal(err)
	}
	if sender.messages[0].CardKind != contracts.CardRoutingError {
		t.Fatalf("unexpected rejection: %+v", sender.messages[0])
	}
}

func TestDefaultTaskCardShowsWorkspaceName(t *testing.T) {
	sender := &fakeSender{ids: []string{"message-1"}}
	n := New(sender)
	if _, err := n.Start(context.Background(), TaskCardInput{
		ChatID: "chat", TaskID: "task-1", Status: "queued", CWDLabel: "/Users/alice/GoProject/codex-feishu-bridge",
	}); err != nil {
		t.Fatal(err)
	}
	if got := fieldValue(sender.messages[0], "项目"); got != "codex-feishu-bridge" {
		t.Fatalf("default task card project = %q", got)
	}
}

type fakeSender struct {
	ids      []string
	messages []contracts.OutboundMessage
}

func (f *fakeSender) Send(_ context.Context, message contracts.OutboundMessage) (contracts.SentMessage, error) {
	f.messages = append(f.messages, message)
	messageID := "message"
	if len(f.ids) > 0 {
		messageID = f.ids[0]
		f.ids = f.ids[1:]
	}
	if message.UpdateMessageID != "" {
		messageID = message.UpdateMessageID
	}
	return contracts.SentMessage{MessageID: messageID}, nil
}

func hasAction(message contracts.OutboundMessage, id string) bool {
	for _, action := range message.Actions {
		if action.ID == id {
			return true
		}
	}
	return false
}

func hasOptionAction(message contracts.OutboundMessage, id string) bool {
	for _, option := range message.Options {
		if option.Action.ID == id {
			return true
		}
	}
	return false
}

func fieldValue(message contracts.OutboundMessage, title string) string {
	for _, field := range message.Fields {
		if field.Title == title {
			return field.Value
		}
	}
	return ""
}
