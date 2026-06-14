package feishu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

func TestReceiverDeliversNormalizedEvents(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "/codex hello"}, "")}},
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
	if len(got) != 1 || got[0].Kind != contracts.InboundNewTask || got[0].Text != "/codex hello" {
		t.Fatalf("unexpected delivered events: %+v", got)
	}
}

func TestReceiverRejectsInvalidEvents(t *testing.T) {
	source := &fakeEventSource{events: []sourceResult{
		{event: RawEvent{Kind: RawEventMessage, Data: messageJSON(t, map[string]any{"text": "/codex hello"}, "")}},
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

type fakeEventSource struct {
	events   []sourceResult
	connects int
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
	return nil
}
