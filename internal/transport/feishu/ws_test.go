package feishu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestReceiverDeliversNormalizedEvents(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "review current changes"}, "")}},
		{err: context.Canceled},
	}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "cli_test", VerificationToken: "verify"}}
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
}

func TestReceiverRejectsInvalidEvents(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "review current changes"}, "")}},
		{err: context.Canceled},
	}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "wrong", VerificationToken: "verify"}}
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

func TestReceiverReconnectsAfterDisconnect(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{err: ErrDisconnected},
		{event: RawEvent{Kind: RawEventCardAction, Data: cardJSON(t, "continue", "")}},
		{err: context.Canceled},
	}}
	r := Receiver{
		Source: source,
		Verify: VerifyOptions{AppID: "cli_test", VerificationToken: "verify"},
		Sleep:  func(ctx context.Context, d time.Duration) error { return nil },
	}
	var got []contracts.InboundEvent
	err := r.Receive(context.Background(), func(ctx context.Context, ev contracts.InboundEvent) error {
		got = append(got, ev)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if source.connects != 2 {
		t.Fatalf("expected reconnect, connects=%d", source.connects)
	}
	if len(got) != 1 || got[0].Kind != contracts.InboundCardAction {
		t.Fatalf("unexpected events: %+v", got)
	}
}

func TestReceiverClosesSourceWhenItStops(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{{err: context.Canceled}}}
	r := Receiver{Source: source, Verify: VerifyOptions{AppID: "cli_test", VerificationToken: "verify"}}
	if err := r.Receive(context.Background(), func(context.Context, contracts.InboundEvent) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if source.closes != 1 {
		t.Fatalf("source should close once, closes=%d", source.closes)
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
		Verify: VerifyOptions{AppID: "cli_test", VerificationToken: "verify"},
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
