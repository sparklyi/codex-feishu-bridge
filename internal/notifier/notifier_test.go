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
	if len(sender.messages) != 1 || sender.messages[0].CardKind != contracts.CardStart || hasAction(sender.messages[0], "steer_submit") || !hasAction(sender.messages[0], "stop_task") || hasAction(sender.messages[0], "continue_submit") {
		t.Fatalf("unexpected start card: %+v", sender.messages)
	}
	if sender.messages[0].Title != "等待处理" || sender.messages[0].Subtitle != "项目：backend" || sender.messages[0].Presentation == nil || sender.messages[0].Presentation.Layout != contracts.TaskCardRunning {
		t.Fatalf("task card should prioritize its semantic header: %+v", sender.messages[0])
	}
	if strings.Contains(sender.messages[0].Subtitle, "/Users/alice/repo") {
		t.Fatalf("absolute paths should be redacted: %q", sender.messages[0].Subtitle)
	}
	if _, err := n.Progress(context.Background(), TaskCardInput{ChatID: "chat", UpdateMessageID: "message-1", TaskID: "task-1", Status: "running", Body: "working"}); err != nil {
		t.Fatal(err)
	}
	if sender.messages[1].UpdateMessageID != "message-1" || sender.messages[1].Status != "running" {
		t.Fatalf("progress must patch running card: %+v", sender.messages[1])
	}
	if !hasAction(sender.messages[1], "steer_submit") || !hasAction(sender.messages[1], "stop_task") {
		t.Fatalf("running card should expose steer and stop: %+v", sender.messages[1].Actions)
	}
	if _, err := n.Success(context.Background(), TaskCardInput{ChatID: "chat", UpdateMessageID: "message-1", TaskID: "task-1", Status: "succeeded", Body: "done"}); err != nil {
		t.Fatal(err)
	}
	if hasAction(sender.messages[2], "stop_task") || !hasAction(sender.messages[2], "continue_submit") || !hasAction(sender.messages[2], "view_details") || sender.messages[2].Presentation == nil || sender.messages[2].Presentation.Layout != contracts.TaskCardResult {
		t.Fatalf("terminal card actions incorrect: %+v", sender.messages[2].Actions)
	}
}

func TestTaskCardDisplayModesAndDetailsPaging(t *testing.T) {
	input := TaskCardInput{
		ChatID: "chat", TaskID: "task-1", Status: "running",
		Presentation: contracts.TaskPresentation{Stage: "整理结果", Activity: "正在整理回复。", Draft: "This is a reply draft that should only appear in preview mode."},
	}
	conciseSender := &fakeSender{ids: []string{"concise"}}
	if _, err := New(conciseSender).Progress(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if got := conciseSender.messages[0].Presentation.Draft; got != "" {
		t.Fatalf("concise mode exposed reply draft: %q", got)
	}
	previewSender := &fakeSender{ids: []string{"preview", "details"}}
	preview := New(previewSender, Options{CardDisplayMode: "preview"})
	if _, err := preview.Progress(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if got := previewSender.messages[0].Presentation.Draft; got == "" {
		t.Fatal("preview mode removed reply draft")
	}
	longDraft := strings.Repeat("草稿", 400)
	if _, err := preview.Progress(context.Background(), TaskCardInput{ChatID: "chat", TaskID: "task-1", Status: "running", Presentation: contracts.TaskPresentation{Draft: longDraft}}); err != nil {
		t.Fatal(err)
	}
	if got := []rune(previewSender.messages[1].Presentation.Draft); len(got) > 600 {
		t.Fatalf("preview draft should stay compact, got %d runes", len(got))
	}
	longResult := strings.Repeat("详细结果内容。", 400)
	if _, err := preview.Details(context.Background(), DetailsInput{ChatID: "chat", TaskID: "task-1", Status: "succeeded", FinalText: longResult, Page: 1}); err != nil {
		t.Fatal(err)
	}
	details := previewSender.messages[2]
	if details.CardKind != contracts.CardDetails || details.Presentation == nil || details.Presentation.Layout != contracts.TaskCardDetails || details.Presentation.DetailPages < 2 || !hasAction(details, "details_page") {
		t.Fatalf("details card did not expose paged result: %+v", details)
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

func TestDefaultTaskCardUsesWorkspaceBaseAsProjectSubtitle(t *testing.T) {
	sender := &fakeSender{ids: []string{"message-1"}}
	n := New(sender)
	if _, err := n.Start(context.Background(), TaskCardInput{
		ChatID: "chat", TaskID: "task-1", Status: "queued", CWDLabel: "/Users/alice/GoProject/codex-feishu-bridge",
	}); err != nil {
		t.Fatal(err)
	}
	message := sender.messages[0]
	if message.Subtitle != "项目：codex-feishu-bridge" || strings.Contains(message.Subtitle, "/Users/alice") {
		t.Fatalf("default task card subtitle = %q", message.Subtitle)
	}
	if strings.Contains(message.Title, "task-1") {
		t.Fatalf("task id should not dominate the task card title: %q", message.Title)
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
