package feishu

import (
	"context"
	"errors"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestReceiverDeliversNormalizedEvents(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "review current changes"}, "")}},
		{err: context.Canceled},
	}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "cli_test"}}
	var got []contracts.InboundEvent
	err := r.Receive(context.Background(), func(ctx context.Context, ev contracts.InboundEvent) error {
		got = append(got, ev)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if len(got) != 1 || got[0].Kind != contracts.InboundNewTask || got[0].Text != "review current changes" {
		t.Fatalf("unexpected delivered events: %+v", got)
	}
	if source.connects != 1 {
		t.Fatalf("receiver should initialize its source once, connects=%d", source.connects)
	}
}

func TestReceiverRejectsInvalidEvents(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "review current changes"}, "")}},
		{err: context.Canceled},
	}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "wrong"}}
	calls := 0
	err := r.Receive(context.Background(), func(ctx context.Context, ev contracts.InboundEvent) error {
		calls++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("handler should not be called for invalid events")
	}
}

func TestReceiverClosesSourceWhenItStops(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{{err: context.Canceled}}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "cli_test"}}
	if err := r.Receive(context.Background(), func(context.Context, contracts.InboundEvent) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if source.closes != 1 {
		t.Fatalf("source should close once, closes=%d", source.closes)
	}
}

func TestReceiverStopsWhenHandlerCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "重启服务"}, "")}},
		{err: context.Canceled},
	}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "cli_test"}}
	err := r.Receive(ctx, func(context.Context, contracts.InboundEvent) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("receiver error = %v, want context canceled", err)
	}
	if source.closes != 1 {
		t.Fatalf("source closes = %d, want 1", source.closes)
	}
}

func TestReceiverContinuesAfterHandlerError(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventCardAction, Data: cardJSON(t, "stop_task", "")}},
		{event: RawEvent{Kind: RawEventCardAction, Data: cardJSON(t, "stop_task", "")}},
		{err: context.Canceled},
	}}
	var handled, reported int
	r := Receiver{
		Source: source,
		Verify: VerifyOptions{AppID: "cli_test"},
		OnHandleError: func(context.Context, contracts.InboundEvent, error) {
			reported++
		},
	}
	err := r.Receive(context.Background(), func(context.Context, contracts.InboundEvent) error {
		handled++
		if handled == 1 {
			return errors.New("temporary action failure")
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if handled != 2 || reported != 1 {
		t.Fatalf("handler errors should not stop the receiver: handled=%d reported=%d", handled, reported)
	}
}

func TestSDKEventSourcePrioritizesCardActions(t *testing.T) {
	source := &SDKEventSource{
		events:      make(chan sourceEvent, 1),
		cardActions: make(chan sourceEvent, 1),
	}
	source.events <- sourceEvent{raw: RawEvent{Kind: RawEventMessage}}
	if !source.tryPublishCardAction(context.Background(), RawEvent{Kind: RawEventCardAction}) {
		t.Fatal("card action should enter its dedicated queue")
	}

	raw, err := source.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if raw.Kind != RawEventCardAction {
		t.Fatalf("card action should take priority, got %q", raw.Kind)
	}
}

func TestSDKEventSourceRejectsFullCardQueueWithoutBlocking(t *testing.T) {
	source := &SDKEventSource{cardActions: make(chan sourceEvent, 1)}
	source.cardActions <- sourceEvent{}
	if source.tryPublishCardAction(context.Background(), RawEvent{Kind: RawEventCardAction}) {
		t.Fatal("full card callback queue should reject the event without blocking")
	}
	accepted := cardActionResponse("success", "操作已收到。")
	if accepted.Toast == nil || accepted.Toast.Type != "success" || accepted.Toast.Content == "" {
		t.Fatalf("missing immediate callback acknowledgement: %+v", accepted)
	}
	busy := cardActionResponse("error", "服务繁忙，请稍后重试。")
	if busy.Toast == nil || busy.Toast.Type != "error" || busy.Toast.Content == "" {
		t.Fatalf("missing callback overload response: %+v", busy)
	}
}

func TestSDKEventSourceRejectsFullMessageQueueWithoutBlocking(t *testing.T) {
	source := &SDKEventSource{events: make(chan sourceEvent, 1)}
	source.events <- sourceEvent{}
	if source.tryPublishMessage(context.Background(), RawEvent{Kind: RawEventMessage}) {
		t.Fatal("full message queue should reject the event without blocking")
	}
}

func TestSDKEventSourceReturnsBackgroundFailure(t *testing.T) {
	want := errors.New("websocket stopped")
	source := &SDKEventSource{
		events:      make(chan sourceEvent, 1),
		cardActions: make(chan sourceEvent, 1),
		failures:    make(chan error, 1),
	}
	source.failures <- want
	_, err := source.Receive(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("receive error = %v, want %v", err, want)
	}
}

func TestSDKEventSourceCloseWithoutClient(t *testing.T) {
	if err := (&SDKEventSource{}).Close(); err != nil {
		t.Fatalf("close without client: %v", err)
	}
}

func TestCardCallbackEnvelopePreservesButtonActionValue(t *testing.T) {
	raw := mustMarshal(cardCallbackEnvelope(&callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
			Context:  &callback.Context{OpenMessageID: "card_msg_1", OpenChatID: "chat_1"},
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{
					"action_id": "stop_task",
					"action":    "stop_task",
					"task_id":   "cx_1",
				},
			},
		},
	}))
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ActionID != "stop_task" || ev.ActionValue["action"] != "stop_task" || ev.ActionValue["task_id"] != "cx_1" {
		t.Fatalf("button action value was not preserved: %+v", ev)
	}
	if ev.Text != "" {
		t.Fatalf("button callback without form input should not synthesize text, got %q", ev.Text)
	}
	if ev.ChatType != "private" {
		t.Fatalf("bridge card callback must preserve private scope, got %q", ev.ChatType)
	}
}

func TestCardCallbackEnvelopeUsesFormValueText(t *testing.T) {
	raw := mustMarshal(cardCallbackEnvelope(&callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
			Context:  &callback.Context{OpenMessageID: "card_msg_1", OpenChatID: "chat_1"},
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{
					"action_id": "continue_submit",
					"action":    "continue",
					"task_id":   "cx_1",
				},
				FormValue: map[string]interface{}{
					"text": "继续检查服务日志",
				},
			},
		},
	}))
	ev, err := NormalizeCardActionJSON(raw, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ActionID != "continue_submit" || ev.ActionValue["action"] != "continue" || ev.Text != "继续检查服务日志" {
		t.Fatalf("form callback text was not preserved: %+v", ev)
	}
}

type fakeEventSource struct {
	events   []sourceResult
	connects int
	closes   int
}

type sourceResult struct {
	event RawEvent
	err   error
}

func (f *fakeEventSource) Connect(ctx context.Context) error {
	f.connects++
	return nil
}

func (f *fakeEventSource) Receive(ctx context.Context) (RawEvent, error) {
	if len(f.events) == 0 {
		return RawEvent{}, context.Canceled
	}
	result := f.events[0]
	f.events = f.events[1:]
	return result.event, result.err
}

func (f *fakeEventSource) Close() error {
	f.closes++
	return nil
}
