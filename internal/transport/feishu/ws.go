package feishu

import (
	"context"
	"encoding/json"
	"errors"
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
)

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
	client *larkws.Client
	events chan sourceEvent
}

type sourceEvent struct {
	raw RawEvent
	err error
}

func NewSDKEventSource(appID, appSecret, verificationToken string) *SDKEventSource {
	source := &SDKEventSource{events: make(chan sourceEvent, 64)}
	eventDispatcher := dispatcher.NewEventDispatcher(verificationToken, "")
	eventDispatcher.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		source.publish(ctx, RawEvent{Kind: RawEventMessage, Data: mustMarshal(event)})
		return nil
	})
	eventDispatcher.OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		source.publish(ctx, RawEvent{Kind: RawEventCardAction, Data: mustMarshal(cardCallbackEnvelope(event))})
		return &callback.CardActionTriggerResponse{}, nil
	})
	source.client = larkws.NewClient(appID, appSecret, larkws.WithEventHandler(eventDispatcher), larkws.WithAutoReconnect(true))
	return source
}

func (s *SDKEventSource) Connect(ctx context.Context) error {
	go func() {
		if err := s.client.Start(ctx); err != nil {
			s.publish(ctx, RawEvent{}, err)
		}
	}()
	return nil
}

func (s *SDKEventSource) Receive(ctx context.Context) (RawEvent, error) {
	select {
	case <-ctx.Done():
		return RawEvent{}, ctx.Err()
	case ev := <-s.events:
		return ev.raw, ev.err
	}
}

func (s *SDKEventSource) Close() error {
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

type Receiver struct {
	Source  EventSource
	Verify  VerifyOptions
	Backoff time.Duration
	Sleep   func(context.Context, time.Duration) error
}

func (r Receiver) Receive(ctx context.Context, handle func(context.Context, contracts.InboundEvent) error) error {
	if r.Source == nil {
		return errors.New("feishu event source is nil")
	}
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
				_ = r.Source.Close()
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
				continue
			}
			if err := handle(ctx, ev); err != nil {
				return err
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
	if req != nil && req.Action != nil {
		if value, ok := req.Action.Value["action_id"].(string); ok {
			actionID = value
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
				"value": map[string]any{
					"text": text,
				},
			},
		},
	}
}
