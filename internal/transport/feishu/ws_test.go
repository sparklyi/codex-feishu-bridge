package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

func TestApplyHTTPTimeout(t *testing.T) {
	tests := []struct {
		name    string
		initial time.Duration
		limit   time.Duration
		want    time.Duration
	}{
		{name: "sets an unbounded client", initial: 0, limit: 15 * time.Second, want: 15 * time.Second},
		{name: "caps a longer timeout", initial: time.Minute, limit: 15 * time.Second, want: 15 * time.Second},
		{name: "keeps a shorter timeout", initial: 3 * time.Second, limit: 15 * time.Second, want: 3 * time.Second},
		{name: "ignores a nonpositive limit", initial: time.Minute, limit: 0, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Timeout: tt.initial}
			applyHTTPTimeout(client, tt.limit)
			if client.Timeout != tt.want {
				t.Fatalf("timeout = %s, want %s", client.Timeout, tt.want)
			}
		})
	}
}

func TestWSBootstrapTransportLimitsOnlyBootstrapRequest(t *testing.T) {
	var bootstrapDeadline time.Time
	var apiHasDeadline bool
	transport := wsBootstrapTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/callback/ws/endpoint" {
			var ok bool
			bootstrapDeadline, ok = req.Context().Deadline()
			if !ok {
				t.Fatal("bootstrap request should have a deadline")
			}
		} else {
			_, apiHasDeadline = req.Context().Deadline()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"ClientConfig":{}}}`)),
		}, nil
	})}

	started := time.Now()
	if _, err := transport.RoundTrip(newRequest(t, "/callback/ws/endpoint")); err != nil {
		t.Fatal(err)
	}
	if bootstrapDeadline.Before(started.Add(wsBootstrapTimeout-time.Second)) || bootstrapDeadline.After(started.Add(wsBootstrapTimeout+time.Second)) {
		t.Fatalf("bootstrap deadline = %s, want approximately %s", bootstrapDeadline.Sub(started), wsBootstrapTimeout)
	}
	if _, err := transport.RoundTrip(newRequest(t, "/open-apis/auth/v3/tenant_access_token/internal")); err != nil {
		t.Fatal(err)
	}
	if apiHasDeadline {
		t.Fatal("normal Feishu API request should not receive the bootstrap deadline")
	}
}

func TestTuneWSBootstrapConfig(t *testing.T) {
	payload := []byte(`{"code":0,"msg":"","data":{"URL":"wss://example.test/?ticket=opaque","ClientConfig":{"ReconnectCount":-1,"ReconnectInterval":90,"ReconnectNonce":25,"PingInterval":90}}}`)
	tuned, ok := tuneWSBootstrapConfig(payload)
	if !ok {
		t.Fatal("expected bootstrap config to be tuned")
	}
	var decoded struct {
		Data struct {
			URL          string
			ClientConfig struct {
				ReconnectCount    int
				ReconnectInterval int
				ReconnectNonce    int
				PingInterval      int
			}
		}
	}
	if err := json.Unmarshal(tuned, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Data.URL != "wss://example.test/?ticket=opaque" {
		t.Fatalf("URL was not preserved: %q", decoded.Data.URL)
	}
	if decoded.Data.ClientConfig.ReconnectCount != -1 || decoded.Data.ClientConfig.ReconnectInterval != sdkReconnectDelay || decoded.Data.ClientConfig.ReconnectNonce != sdkReconnectNonce || decoded.Data.ClientConfig.PingInterval != sdkPingInterval {
		t.Fatalf("unexpected tuned config: %+v", decoded.Data.ClientConfig)
	}
}

func TestTuneWSBootstrapConfigRejectsInvalidPayload(t *testing.T) {
	payload := []byte(`not-json`)
	tuned, ok := tuneWSBootstrapConfig(payload)
	if ok || string(tuned) != string(payload) {
		t.Fatalf("invalid payload should remain unchanged: %q", tuned)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://open.feishu.cn"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
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
