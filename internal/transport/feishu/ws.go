package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

const (
	RawEventMessage    = "message"
	RawEventCardAction = "card_action"
	sourceCloseTimeout = 5 * time.Second
)

var errMessageQueueFull = errors.New("feishu message queue is full")

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
	client      *feishuWSClient
	events      chan sourceEvent
	cardActions chan sourceEvent
	failures    chan error
	startOnce   sync.Once
	mu          sync.Mutex
	done        chan struct{}
}

type sourceEvent struct {
	raw RawEvent
}

func NewSDKEventSource(appID, appSecret string, proxyURL *url.URL) *SDKEventSource {
	source := &SDKEventSource{
		events:      make(chan sourceEvent, 64),
		cardActions: make(chan sourceEvent, 64),
		failures:    make(chan error, 1),
	}
	eventDispatcher := dispatcher.NewEventDispatcher("", "")
	eventDispatcher.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		slog.Info("Feishu message received")
		if !source.tryPublishMessage(ctx, RawEvent{Kind: RawEventMessage, Data: mustMarshal(event)}) {
			slog.Warn("Feishu message rejected because the event queue is full")
			return errMessageQueueFull
		}
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
	source.client = newFeishuWSClient(appID, appSecret, eventDispatcher, proxyURL)
	return source
}

func (s *SDKEventSource) Connect(ctx context.Context) error {
	if s.client == nil {
		return errors.New("feishu websocket client is nil")
	}
	s.startOnce.Do(func() {
		done := make(chan struct{})
		s.mu.Lock()
		s.done = done
		s.mu.Unlock()
		go func() {
			defer close(done)
			if err := s.client.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.publishFailure(ctx, err)
			}
		}()
	})
	return nil
}

func (s *SDKEventSource) Receive(ctx context.Context) (RawEvent, error) {
	if s.cardActions != nil {
		select {
		case ev := <-s.cardActions:
			return ev.raw, nil
		default:
		}
	}
	select {
	case <-ctx.Done():
		return RawEvent{}, ctx.Err()
	case err := <-s.failures:
		return RawEvent{}, err
	case ev := <-s.cardActions:
		return ev.raw, nil
	case ev := <-s.events:
		return ev.raw, nil
	}
}

func (s *SDKEventSource) Close() error {
	if s.client == nil {
		return nil
	}
	closeErr := s.client.Close()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return closeErr
	}
	timer := time.NewTimer(sourceCloseTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return closeErr
	case <-timer.C:
		return errors.Join(closeErr, errors.New("feishu event source did not stop before timeout"))
	}
}

func (s *SDKEventSource) tryPublishMessage(ctx context.Context, raw RawEvent) bool {
	if s.events == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case s.events <- sourceEvent{raw: raw}:
		return true
	default:
		return false
	}
}

func (s *SDKEventSource) publishFailure(ctx context.Context, err error) {
	if err == nil || s.failures == nil {
		return
	}
	select {
	case <-ctx.Done():
	case s.failures <- err:
	}
}

func (s *SDKEventSource) tryPublishCardAction(ctx context.Context, raw RawEvent) bool {
	if s.cardActions == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case s.cardActions <- sourceEvent{raw: raw}:
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
	OnHandleError func(context.Context, contracts.InboundEvent, error)
}

func (r Receiver) Receive(ctx context.Context, handle func(context.Context, contracts.InboundEvent) error) error {
	if r.Source == nil {
		return errors.New("feishu event source is nil")
	}
	defer func() {
		if err := r.Source.Close(); err != nil {
			slog.Warn("Feishu event source close failed", "error", err)
		}
	}()
	if err := r.Source.Connect(ctx); err != nil {
		return err
	}
	for {
		raw, err := r.Source.Receive(ctx)
		if err != nil {
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
	// Feishu card callbacks do not include the chat type. The bridge only emits
	// actionable cards from private-chat paths, and task actions additionally
	// validate the stored chat ID and task creator in the router.
	message := map[string]any{
		"message_id": contextMap["open_message_id"],
		"chat_id":    contextMap["open_chat_id"],
		"chat_type":  "private",
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
