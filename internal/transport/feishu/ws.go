package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

var ErrDisconnected = errors.New("feishu websocket disconnected")

const (
	RawEventMessage    = "message"
	RawEventCardAction = "card_action"
	sdkHTTPTimeout     = 30 * time.Second
	wsBootstrapTimeout = 8 * time.Second
	sdkReconnectDelay  = 1
	sdkReconnectNonce  = 0
)

var sdkHTTPTimeoutOnce sync.Once

type RawEvent struct {
	Kind string
	Data []byte
}

type EventSource interface {
	Connect(ctx context.Context) error
	Receive(ctx context.Context) (RawEvent, error)
	Close() error
}

type SDKEventSource struct {
	client      *larkws.Client
	events      chan sourceEvent
	cardActions chan sourceEvent
	startOnce   sync.Once
}

type sourceEvent struct {
	raw RawEvent
	err error
}

func NewSDKEventSource(appID, appSecret, verificationToken string) *SDKEventSource {
	configureSDKHTTPTimeout()
	source := &SDKEventSource{
		events:      make(chan sourceEvent, 64),
		cardActions: make(chan sourceEvent, 64),
	}
	eventDispatcher := dispatcher.NewEventDispatcher(verificationToken, "")
	eventDispatcher.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		slog.Info("Feishu message received")
		source.publish(ctx, RawEvent{Kind: RawEventMessage, Data: mustMarshal(event)})
		return nil
	})
	eventDispatcher.OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		if !source.tryPublishCardAction(ctx, RawEvent{Kind: RawEventCardAction, Data: mustMarshal(cardCallbackEnvelope(event))}) {
			slog.Warn("Feishu card action dropped because the callback queue is full")
			return cardActionResponse("error", "服务繁忙，请稍后重试。"), nil
		}
		slog.Info("Feishu card action received")
		return cardActionResponse("success", "操作已收到。"), nil
	})
	source.client = larkws.NewClient(appID, appSecret, larkws.WithEventHandler(eventDispatcher), larkws.WithAutoReconnect(true))
	return source
}

// The upstream WebSocket SDK uses http.DefaultClient to obtain a connection
// endpoint but does not expose a retry policy. Bound endpoint requests and
// tighten only that response's reconnect policy so transient network failures
// do not leave the bridge offline for the SDK's 90-second backoff.
func configureSDKHTTPTimeout() {
	sdkHTTPTimeoutOnce.Do(func() {
		applyHTTPTimeout(http.DefaultClient, sdkHTTPTimeout)
		base := http.DefaultClient.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		http.DefaultClient.Transport = wsBootstrapTransport{base: base}
	})
}

func applyHTTPTimeout(client *http.Client, timeout time.Duration) {
	if client == nil || timeout <= 0 {
		return
	}
	if client.Timeout == 0 || client.Timeout > timeout {
		client.Timeout = timeout
	}
}

type wsBootstrapTransport struct {
	base http.RoundTripper
}

func (t wsBootstrapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/callback/ws/endpoint" {
		return t.base.RoundTrip(req)
	}

	ctx, cancel := context.WithTimeout(req.Context(), wsBootstrapTimeout)
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil || resp == nil || resp.Body == nil {
		cancel()
		return resp, err
	}
	payload, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	cancel()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if tuned, ok := tuneWSBootstrapConfig(payload); ok {
		payload = tuned
	}
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.ContentLength = int64(len(payload))
	resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	return resp, nil
}

func tuneWSBootstrapConfig(payload []byte) ([]byte, bool) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(payload, &response); err != nil {
		return payload, false
	}
	dataRaw, ok := response["data"]
	if !ok {
		return payload, false
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		return payload, false
	}
	configRaw, ok := data["ClientConfig"]
	if !ok {
		return payload, false
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(configRaw, &config); err != nil {
		return payload, false
	}
	config["ReconnectInterval"] = json.RawMessage(strconv.Itoa(sdkReconnectDelay))
	config["ReconnectNonce"] = json.RawMessage(strconv.Itoa(sdkReconnectNonce))
	tunedConfig, err := json.Marshal(config)
	if err != nil {
		return payload, false
	}
	data["ClientConfig"] = tunedConfig
	tunedData, err := json.Marshal(data)
	if err != nil {
		return payload, false
	}
	response["data"] = tunedData
	tuned, err := json.Marshal(response)
	if err != nil {
		return payload, false
	}
	return tuned, true
}

func (s *SDKEventSource) Connect(ctx context.Context) error {
	if s.client == nil {
		return errors.New("feishu websocket client is nil")
	}
	s.startOnce.Do(func() {
		go func() {
			if err := s.client.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.publish(ctx, RawEvent{}, err)
			}
		}()
	})
	return nil
}

func (s *SDKEventSource) Receive(ctx context.Context) (RawEvent, error) {
	if s.cardActions != nil {
		select {
		case ev := <-s.cardActions:
			return ev.raw, ev.err
		default:
		}
	}
	select {
	case <-ctx.Done():
		return RawEvent{}, ctx.Err()
	case ev := <-s.cardActions:
		return ev.raw, ev.err
	case ev := <-s.events:
		return ev.raw, ev.err
	}
}

func (s *SDKEventSource) Close() error {
	if s.client != nil {
		s.client.Close()
	}
	return nil
}

func (s *SDKEventSource) publish(ctx context.Context, raw RawEvent, errs ...error) {
	var err error
	if len(errs) > 0 {
		err = errs[0]
	}
	select {
	case <-ctx.Done():
	case s.events <- sourceEvent{raw: raw, err: err}:
	}
}

func (s *SDKEventSource) tryPublishCardAction(ctx context.Context, raw RawEvent, errs ...error) bool {
	if s.cardActions == nil {
		return false
	}
	var err error
	if len(errs) > 0 {
		err = errs[0]
	}
	select {
	case <-ctx.Done():
		return false
	case s.cardActions <- sourceEvent{raw: raw, err: err}:
		return true
	default:
		return false
	}
}

func cardActionResponse(kind, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: content}}
}

type Receiver struct {
	Source        EventSource
	Verify        VerifyOptions
	Backoff       time.Duration
	Sleep         func(context.Context, time.Duration) error
	OnHandleError func(context.Context, contracts.InboundEvent, error)
}

func (r Receiver) Receive(ctx context.Context, handle func(context.Context, contracts.InboundEvent) error) error {
	if r.Source == nil {
		return errors.New("feishu event source is nil")
	}
	defer r.Source.Close()
	backoff := r.Backoff
	if backoff == 0 {
		backoff = 500 * time.Millisecond
	}
	for {
		if err := r.Source.Connect(ctx); err != nil {
			return err
		}
		for {
			raw, err := r.Source.Receive(ctx)
			if err != nil {
				if errors.Is(err, ErrDisconnected) {
					if sleepErr := r.sleep(ctx, backoff); sleepErr != nil {
						return sleepErr
					}
					break
				}
				return err
			}
			ev, err := r.normalize(raw)
			if err != nil {
				if raw.Kind == RawEventCardAction {
					slog.Warn("Feishu card action rejected", "error", err)
				}
				continue
			}
			slog.Info("Feishu inbound event dispatched", "kind", ev.Kind)
			if err := handle(ctx, ev); err != nil {
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return ctx.Err()
				}
				if r.OnHandleError != nil {
					r.OnHandleError(ctx, ev, err)
				}
			}
		}
	}
}

func (r Receiver) normalize(raw RawEvent) (contracts.InboundEvent, error) {
	switch raw.Kind {
	case RawEventMessage:
		return NormalizeMessageJSON(raw.Data, r.Verify)
	case RawEventCardAction:
		return NormalizeCardActionJSON(raw.Data, r.Verify)
	default:
		return contracts.InboundEvent{}, errors.New("unknown raw Feishu event kind")
	}
}

func (r Receiver) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return data
}

func cardCallbackEnvelope(event *callback.CardActionTriggerEvent) map[string]any {
	var header any
	if event.EventV2Base != nil {
		header = event.EventV2Base.Header
	}
	req := event.Event
	actionID := ""
	text := ""
	actionValue := map[string]any{}
	if req != nil && req.Action != nil {
		if value, ok := req.Action.Value["action_id"].(string); ok {
			actionID = value
		}
		for key, value := range req.Action.Value {
			actionValue[key] = value
		}
		if value, ok := req.Action.Value["text"].(string); ok {
			text = value
		}
		if value, ok := req.Action.FormValue["text"].(string); ok {
			text = value
		}
		if text == "" {
			text = req.Action.InputValue
		}
		if actionID == "" {
			actionID = req.Action.Name
		}
	}
	if text != "" {
		actionValue["text"] = text
	}
	operator := map[string]any{}
	contextMap := map[string]any{}
	if req != nil {
		if req.Operator != nil {
			operator["open_id"] = req.Operator.OpenID
		}
		if req.Context != nil {
			contextMap["open_message_id"] = req.Context.OpenMessageID
			contextMap["open_chat_id"] = req.Context.OpenChatID
		}
	}
	message := map[string]any{
		"message_id": contextMap["open_message_id"],
		"chat_id":    contextMap["open_chat_id"],
		"chat_type":  "unknown",
	}
	return map[string]any{
		"header": header,
		"event": map[string]any{
			"operator": operator,
			"context":  contextMap,
			"message":  message,
			"action": map[string]any{
				"action_id": actionID,
				"value":     actionValue,
			},
		},
	}
}
