package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport"
)

// ErrRateLimited is retained for callers of the Feishu transport. It is the
// shared transport sentinel so runtime delivery retries classify it correctly.
var ErrRateLimited = transport.ErrRateLimited

const (
	defaultDeliveryAttemptTimeout = 5 * time.Second
	defaultDeliveryMaxRetries     = 2
	defaultDeliveryRetryDelay     = 100 * time.Millisecond
	cardStreamSetupTimeout        = 2 * time.Second
	taskProcessingDetailElementID = "task_stream_detail"
	taskStreamPrintFrequencyMS    = 30
	taskStreamPrintStep           = 1
	cardStreamSequenceBlockSize   = 8192
	maxCardStreamSequence         = 2147483647
)

type CardAPI interface {
	SendCard(ctx context.Context, chatID, replyToMessageID string, cardJSON []byte) (messageID string, retryAfter time.Duration, err error)
	PatchCard(ctx context.Context, messageID string, cardJSON []byte) (retryAfter time.Duration, err error)
}

// CardStreamAPI is the optional CardKit surface for updating just the
// processing-detail component of an IM card. The regular CardAPI remains the
// compatibility path for clients or applications without CardKit access.
type CardStreamAPI interface {
	ResolveCardID(ctx context.Context, messageID string) (string, error)
	SetCardStreaming(ctx context.Context, cardID string, enabled bool, sequence int) error
	SetCardElementContent(ctx context.Context, cardID, elementID, content string, sequence int) error
}

// CardStreamSequenceAllocator reserves a sequence range before a CardKit
// stream begins. It keeps a reused task card valid across bridge restarts.
type CardStreamSequenceAllocator interface {
	ReserveCardStreamSequence(ctx context.Context, messageID string, minimum, count int) (int, error)
}

type Sender struct {
	AppID             string
	AppSecret         string
	API               CardAPI
	MaxAttempts       int
	MaxRetries        int
	AttemptTimeout    time.Duration
	RetryDelay        time.Duration
	Sleep             func(context.Context, time.Duration) error
	SequenceAllocator CardStreamSequenceAllocator

	streamMu sync.Mutex
	streams  map[string]*taskCardStream
}

type taskCardStream struct {
	mu           sync.Mutex
	cardID       string
	nextSequence int
	sequenceEnd  int
	lastKey      string
	lastDetail   string
	active       bool
	unavailable  bool
}

// SenderOptions controls retry behavior for Feishu card delivery.
type SenderOptions struct {
	MaxAttempts       int
	AttemptTimeout    time.Duration
	RetryDelay        time.Duration
	SequenceAllocator CardStreamSequenceAllocator
}

func NewSenderFromEnv(appID, secretEnv string, getenv func(string) string, api CardAPI) (*Sender, error) {
	return NewSenderFromEnvWithOptions(appID, secretEnv, getenv, api, SenderOptions{})
}

func NewSenderFromEnvWithOptions(appID, secretEnv string, getenv func(string) string, api CardAPI, options SenderOptions) (*Sender, error) {
	if getenv == nil {
		return nil, errors.New("getenv is required")
	}
	secret := getenv(secretEnv)
	if secret == "" {
		return nil, fmt.Errorf("missing Feishu app secret env %s", secretEnv)
	}
	return &Sender{
		AppID:             appID,
		AppSecret:         secret,
		API:               api,
		MaxAttempts:       options.MaxAttempts,
		AttemptTimeout:    options.AttemptTimeout,
		RetryDelay:        options.RetryDelay,
		SequenceAllocator: options.SequenceAllocator,
	}, nil
}

func NewSDKCardAPI(appID, appSecret string, proxyURL *url.URL) *SDKCardAPI {
	return NewSDKCardAPIWithOptions(appID, appSecret, proxyURL, NetworkOptions{})
}

func NewSDKCardAPIWithOptions(appID, appSecret string, proxyURL *url.URL, options NetworkOptions) *SDKCardAPI {
	httpClient := newFeishuHTTPClientWithOptions(options, proxyURL)
	return &SDKCardAPI{
		client:     lark.NewClient(appID, appSecret, lark.WithHttpClient(httpClient)),
		httpClient: httpClient,
	}
}

type SDKCardAPI struct {
	client     *lark.Client
	httpClient *http.Client
}

func (api *SDKCardAPI) SendCard(ctx context.Context, chatID, replyToMessageID string, cardJSON []byte) (string, time.Duration, error) {
	content := string(cardJSON)
	if replyToMessageID != "" {
		body := larkim.NewReplyMessageReqBodyBuilder().
			MsgType("interactive").
			Content(content).
			Build()
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(replyToMessageID).
			Body(body).
			Build()
		resp, err := api.client.Im.Message.Reply(ctx, req)
		if err != nil {
			api.closeIdleConnections()
			return "", 0, err
		}
		if !resp.Success() {
			return "", 0, feishuResponseError("reply", resp.Code, resp.Msg)
		}
		if resp.Data == nil || resp.Data.MessageId == nil {
			return "", 0, nil
		}
		return *resp.Data.MessageId, 0, nil
	}
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(chatID).
		MsgType("interactive").
		Content(content).
		Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(body).
		Build()
	resp, err := api.client.Im.Message.Create(ctx, req)
	if err != nil {
		api.closeIdleConnections()
		return "", 0, err
	}
	if !resp.Success() {
		return "", 0, feishuResponseError("send", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", 0, nil
	}
	return *resp.Data.MessageId, 0, nil
}

func (api *SDKCardAPI) PatchCard(ctx context.Context, messageID string, cardJSON []byte) (time.Duration, error) {
	body := larkim.NewPatchMessageReqBodyBuilder().
		Content(string(cardJSON)).
		Build()
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()
	resp, err := api.client.Im.Message.Patch(ctx, req)
	if err != nil {
		api.closeIdleConnections()
		return 0, err
	}
	if !resp.Success() {
		return 0, feishuResponseError("patch", resp.Code, resp.Msg)
	}
	return 0, nil
}

func (api *SDKCardAPI) ResolveCardID(ctx context.Context, messageID string) (string, error) {
	request := larkcardkit.NewIdConvertCardReqBuilder().
		Body(larkcardkit.NewIdConvertCardReqBodyBuilder().MessageId(messageID).Build()).
		Build()
	response, err := api.client.Cardkit.V1.Card.IdConvert(ctx, request)
	if err != nil {
		api.closeIdleConnections()
		return "", err
	}
	if !response.Success() {
		return "", feishuResponseError("card id conversion", response.Code, response.Msg)
	}
	if response.Data == nil || response.Data.CardId == nil || *response.Data.CardId == "" {
		return "", errors.New("feishu card id conversion returned empty card id")
	}
	return *response.Data.CardId, nil
}

func (api *SDKCardAPI) SetCardStreaming(ctx context.Context, cardID string, enabled bool, sequence int) error {
	settings, err := json.Marshal(taskStreamSettings(enabled))
	if err != nil {
		return err
	}
	request := larkcardkit.NewSettingsCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().
			Settings(string(settings)).
			Uuid(cardStreamOperationID(cardID, sequence)).
			Sequence(sequence).
			Build()).
		Build()
	response, err := api.client.Cardkit.V1.Card.Settings(ctx, request)
	if err != nil {
		api.closeIdleConnections()
		return err
	}
	if !response.Success() {
		return feishuResponseError("card streaming settings", response.Code, response.Msg)
	}
	return nil
}

func taskStreamSettings(enabled bool) map[string]any {
	config := map[string]any{"streaming_mode": enabled}
	if enabled {
		// Fast mode prevents a bursty model response from building an ever-growing
		// client-side typewriter queue between the bridge's coalesced updates.
		config["streaming_config"] = map[string]any{
			"print_frequency_ms": map[string]int{"default": taskStreamPrintFrequencyMS},
			"print_step":         map[string]int{"default": taskStreamPrintStep},
			"print_strategy":     "fast",
		}
	}
	return map[string]any{"config": config}
}

func (api *SDKCardAPI) SetCardElementContent(ctx context.Context, cardID, elementID, content string, sequence int) error {
	payload, err := cardStreamContentPayload(content)
	if err != nil {
		return err
	}
	request := larkcardkit.NewContentCardElementReqBuilder().
		CardId(cardID).
		ElementId(elementID).
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().
			Content(payload).
			Uuid(cardStreamOperationID(cardID, sequence)).
			Sequence(sequence).
			Build()).
		Build()
	response, err := api.client.Cardkit.V1.CardElement.Content(ctx, request)
	if err != nil {
		api.closeIdleConnections()
		return err
	}
	if !response.Success() {
		return feishuResponseError("card stream content", response.Code, response.Msg)
	}
	return nil
}

func cardStreamContentPayload(content string) (string, error) {
	payload, err := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: content})
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// closeIdleConnections prevents a timed-out proxy tunnel from being reused by
// the next coalesced progress patch.
func (api *SDKCardAPI) closeIdleConnections() {
	if api.httpClient != nil {
		api.httpClient.CloseIdleConnections()
	}
}

func (s *Sender) Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error) {
	if s.API == nil {
		return contracts.SentMessage{}, errors.New("feishu sender API is nil")
	}
	card, err := BuildInteractiveCard(msg)
	if err != nil {
		return contracts.SentMessage{}, err
	}
	if msg.UpdateMessageID != "" {
		if isTaskStreamProgress(msg) {
			return s.streamTaskProgress(ctx, msg, card)
		}
		s.closeTaskStream(ctx, msg.UpdateMessageID)
		if err := s.patchWithRetry(ctx, msg.UpdateMessageID, card, msg.DeliveryMaxAttempts); err != nil {
			return contracts.SentMessage{}, err
		}
		return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
	}
	maxRetries := s.maxRetries(msg.DeliveryMaxAttempts)
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return contracts.SentMessage{}, err
		}
		attemptCtx, cancel := s.attemptContext(ctx)
		messageID, retryAfter, err := s.API.SendCard(attemptCtx, msg.ChatID, msg.ReplyToMessageID, card)
		cancel()
		if err == nil {
			if messageID == "" {
				return contracts.SentMessage{}, errors.New("feishu send returned empty message id")
			}
			sent := contracts.SentMessage{MessageID: messageID}
			if isTaskStreamStart(msg) {
				s.startTaskStream(ctx, msg, sent.MessageID)
			}
			return sent, nil
		}
		lastErr = err
		if !shouldRetrySendError(err) || attempt == maxRetries {
			return contracts.SentMessage{}, err
		}
		if err := ctx.Err(); err != nil {
			return contracts.SentMessage{}, err
		}
		if retryAfter <= 0 {
			retryAfter = s.retryDelay(attempt)
		}
		if err := s.sleep(ctx, retryAfter); err != nil {
			return contracts.SentMessage{}, err
		}
	}
	return contracts.SentMessage{}, lastErr
}

func isTaskStreamProgress(msg contracts.OutboundMessage) bool {
	return msg.StreamDetail && msg.CardKind == contracts.CardStart && msg.UpdateMessageID != "" && msg.Presentation != nil && msg.Presentation.Layout == contracts.TaskCardRunning
}

func isTaskStreamStart(msg contracts.OutboundMessage) bool {
	return msg.StreamDetail && msg.CardKind == contracts.CardStart && msg.UpdateMessageID == "" && msg.Presentation != nil && msg.Presentation.Layout == contracts.TaskCardRunning
}

func (s *Sender) startTaskStream(ctx context.Context, msg contracts.OutboundMessage, messageID string) {
	api, ok := s.API.(CardStreamAPI)
	if !ok {
		return
	}
	stream := s.taskStream(messageID)
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.lastKey = taskStreamKey(msg)
	stream.lastDetail = msg.Presentation.ProcessingDetail
	setupCtx, cancel := context.WithTimeout(ctx, cardStreamSetupTimeout)
	defer cancel()
	if err := s.enableTaskStream(setupCtx, api, messageID, stream); err != nil {
		if transport.IsTransientError(err) {
			slog.Warn("Feishu CardKit stream setup delayed; will retry", "message_id", messageID, "error", err)
			return
		}
		stream.unavailable = true
		slog.Warn("Feishu CardKit stream unavailable; using card patches", "message_id", messageID, "error", err)
		return
	}
	slog.Info("Feishu CardKit stream enabled", "message_id", messageID)
}

func (s *Sender) streamTaskProgress(ctx context.Context, msg contracts.OutboundMessage, card []byte) (contracts.SentMessage, error) {
	streamAPI, ok := s.API.(CardStreamAPI)
	if !ok {
		if err := s.patchWithRetry(ctx, msg.UpdateMessageID, card, msg.DeliveryMaxAttempts); err != nil {
			return contracts.SentMessage{}, err
		}
		return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
	}
	stream := s.taskStream(msg.UpdateMessageID)
	stream.mu.Lock()
	defer stream.mu.Unlock()

	key := taskStreamKey(msg)
	detail := msg.Presentation.ProcessingDetail
	if stream.unavailable {
		return s.patchTaskProgress(ctx, msg, card, stream, key, detail)
	}
	if !stream.active {
		if sent, err := s.patchTaskProgress(ctx, msg, card, stream, key, detail); err != nil {
			return sent, err
		}
		if err := s.enableTaskStream(ctx, streamAPI, msg.UpdateMessageID, stream); err != nil {
			if transport.IsTransientError(err) {
				return contracts.SentMessage{}, err
			}
			stream.unavailable = true
			slog.Warn("Feishu CardKit stream unavailable; using card patches", "message_id", msg.UpdateMessageID, "error", err)
		} else {
			slog.Info("Feishu CardKit stream enabled", "message_id", msg.UpdateMessageID)
		}
		return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
	}
	// CardKit only applies a typewriter effect when the next value extends the
	// current value. Redaction or a bounded display buffer can occasionally
	// require a replacement, which must be patched outside streaming mode.
	if key != stream.lastKey || !strings.HasPrefix(detail, stream.lastDetail) {
		if err := s.pauseTaskStream(ctx, streamAPI, msg.UpdateMessageID, stream); err != nil {
			if transport.IsTransientError(err) {
				return contracts.SentMessage{}, err
			}
			s.disableTaskStreamForFallback(ctx, streamAPI, msg.UpdateMessageID, stream, "pause", err)
			return s.patchTaskProgress(ctx, msg, card, stream, key, detail)
		}
		if sent, err := s.patchTaskProgress(ctx, msg, card, stream, key, detail); err != nil {
			return sent, err
		}
		if err := s.enableTaskStream(ctx, streamAPI, msg.UpdateMessageID, stream); err != nil {
			if transport.IsTransientError(err) {
				return contracts.SentMessage{}, err
			}
			stream.unavailable = true
			slog.Warn("Feishu CardKit stream resume failed; using card patches", "message_id", msg.UpdateMessageID, "error", err)
		} else {
			slog.Info("Feishu CardKit stream resumed", "message_id", msg.UpdateMessageID)
		}
		return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
	}
	if detail == stream.lastDetail {
		return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
	}
	sequence, err := s.nextTaskStreamSequence(ctx, msg.UpdateMessageID, stream)
	if err != nil {
		return contracts.SentMessage{}, err
	}
	if err := streamAPI.SetCardElementContent(ctx, stream.cardID, taskProcessingDetailElementID, detail, sequence); err != nil {
		if transport.IsTransientError(err) {
			return contracts.SentMessage{}, err
		}
		s.disableTaskStreamForFallback(ctx, streamAPI, msg.UpdateMessageID, stream, "content update", err)
		return s.patchTaskProgress(ctx, msg, card, stream, key, detail)
	}
	stream.lastDetail = detail
	return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
}

func (s *Sender) patchTaskProgress(ctx context.Context, msg contracts.OutboundMessage, card []byte, stream *taskCardStream, key, detail string) (contracts.SentMessage, error) {
	if err := s.patchWithRetry(ctx, msg.UpdateMessageID, card, msg.DeliveryMaxAttempts); err != nil {
		return contracts.SentMessage{}, err
	}
	stream.lastKey = key
	stream.lastDetail = detail
	return contracts.SentMessage{MessageID: msg.UpdateMessageID}, nil
}

func (s *Sender) enableTaskStream(ctx context.Context, api CardStreamAPI, messageID string, stream *taskCardStream) error {
	if stream.cardID == "" {
		cardID, err := api.ResolveCardID(ctx, messageID)
		if err != nil {
			return err
		}
		stream.cardID = cardID
	}
	sequence, err := s.nextTaskStreamSequence(ctx, messageID, stream)
	if err != nil {
		return err
	}
	if err := api.SetCardStreaming(ctx, stream.cardID, true, sequence); err != nil {
		return err
	}
	stream.active = true
	return nil
}

func (s *Sender) pauseTaskStream(ctx context.Context, api CardStreamAPI, messageID string, stream *taskCardStream) error {
	if !stream.active {
		return nil
	}
	sequence, err := s.nextTaskStreamSequence(ctx, messageID, stream)
	if err != nil {
		return err
	}
	if err := api.SetCardStreaming(ctx, stream.cardID, false, sequence); err != nil {
		return err
	}
	stream.active = false
	return nil
}

func (s *Sender) disableTaskStreamForFallback(ctx context.Context, api CardStreamAPI, messageID string, stream *taskCardStream, operation string, cause error) {
	stream.unavailable = true
	if err := s.pauseTaskStream(ctx, api, messageID, stream); err != nil {
		slog.Warn("Feishu CardKit stream close failed before card patch fallback", "message_id", messageID, "operation", operation, "error", err)
	}
	slog.Warn("Feishu CardKit stream unavailable; using card patches", "message_id", messageID, "operation", operation, "error", cause)
}

func (s *Sender) closeTaskStream(ctx context.Context, messageID string) {
	stream := s.removeTaskStream(messageID)
	if stream == nil {
		return
	}
	api, ok := s.API.(CardStreamAPI)
	if !ok {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if err := s.pauseTaskStream(ctx, api, messageID, stream); err != nil {
		slog.Warn("Feishu CardKit stream close failed", "message_id", messageID, "error", err)
	}
}

func (s *Sender) taskStream(messageID string) *taskCardStream {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.streams == nil {
		s.streams = make(map[string]*taskCardStream)
	}
	if stream := s.streams[messageID]; stream != nil {
		return stream
	}
	stream := &taskCardStream{}
	s.streams[messageID] = stream
	return stream
}

func (s *Sender) removeTaskStream(messageID string) *taskCardStream {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	stream := s.streams[messageID]
	delete(s.streams, messageID)
	return stream
}

func (s *Sender) nextTaskStreamSequence(ctx context.Context, messageID string, stream *taskCardStream) (int, error) {
	if stream.nextSequence == 0 || stream.nextSequence > stream.sequenceEnd {
		start, end, err := s.reserveTaskStreamSequence(ctx, messageID)
		if err != nil {
			return 0, err
		}
		stream.nextSequence = start
		stream.sequenceEnd = end
	}
	sequence := stream.nextSequence
	stream.nextSequence++
	return sequence, nil
}

func (s *Sender) reserveTaskStreamSequence(ctx context.Context, messageID string) (int, int, error) {
	if s.SequenceAllocator == nil {
		// Compatibility for callers that construct Sender directly. The bridge
		// injects a durable allocator in app.Serve.
		return 1, maxCardStreamSequence, nil
	}
	minimum := int(time.Now().Unix())
	if minimum < 1 || minimum > maxCardStreamSequence {
		return 0, 0, errors.New("current time cannot seed a CardKit stream sequence")
	}
	start, err := s.SequenceAllocator.ReserveCardStreamSequence(ctx, messageID, minimum, cardStreamSequenceBlockSize)
	if err != nil {
		return 0, 0, err
	}
	if start < 1 || start > maxCardStreamSequence || cardStreamSequenceBlockSize-1 > maxCardStreamSequence-start {
		return 0, 0, errors.New("CardKit sequence allocator returned an invalid range")
	}
	return start, start + cardStreamSequenceBlockSize - 1, nil
}

func taskStreamKey(msg contracts.OutboundMessage) string {
	state := struct {
		CardKind     contracts.CardKind
		TaskID       string
		Status       string
		Title        string
		Subtitle     string
		Presentation *contracts.TaskPresentation
		Actions      []contracts.Action
	}{
		CardKind: msg.CardKind,
		TaskID:   msg.TaskID,
		Status:   msg.Status,
		Title:    msg.Title,
		Subtitle: msg.Subtitle,
		Actions:  msg.Actions,
	}
	if msg.Presentation != nil {
		presentation := *msg.Presentation
		presentation.ProcessingDetail = ""
		state.Presentation = &presentation
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Sprintf("%#v", state)
	}
	return string(data)
}

func cardStreamOperationID(cardID string, sequence int) string {
	return "codex-stream-" + cardID + "-" + strconv.Itoa(sequence)
}

func (s *Sender) patchWithRetry(ctx context.Context, messageID string, card []byte, maxAttempts int) error {
	maxRetries := s.maxRetries(maxAttempts)
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := s.attemptContext(ctx)
		retryAfter, err := s.API.PatchCard(attemptCtx, messageID, card)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !shouldRetrySendError(err) || attempt == maxRetries {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if retryAfter <= 0 {
			retryAfter = s.retryDelay(attempt)
		}
		if err := s.sleep(ctx, retryAfter); err != nil {
			return err
		}
	}
	return lastErr
}

func shouldRetrySendError(err error) bool {
	return errors.Is(err, ErrRateLimited) || transport.IsTransientError(err)
}

func feishuResponseError(operation string, code int, message string) error {
	if isRateLimitedCode(code) {
		return fmt.Errorf("%w: Feishu %s failed: code=%d msg=%s", ErrRateLimited, operation, code, message)
	}
	return fmt.Errorf("feishu %s failed: code=%d msg=%s", operation, code, message)
}

func isRateLimitedCode(code int) bool {
	return code == 99991400 || code == 99991401 || code == 230002
}

func (s *Sender) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.AttemptTimeout
	if timeout <= 0 {
		timeout = defaultDeliveryAttemptTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *Sender) maxRetries(maxAttempts int) int {
	if maxAttempts > 0 {
		return maxAttempts - 1
	}
	if s.MaxAttempts > 0 {
		return s.MaxAttempts - 1
	}
	if s.MaxRetries > 0 {
		return s.MaxRetries
	}
	return defaultDeliveryMaxRetries
}

func (s *Sender) retryDelay(attempt int) time.Duration {
	delay := s.RetryDelay
	if delay <= 0 {
		delay = defaultDeliveryRetryDelay
	}
	return time.Duration(attempt+1) * delay
}

func BuildInteractiveCard(msg contracts.OutboundMessage) ([]byte, error) {
	if msg.Presentation != nil && msg.Presentation.Layout != "" {
		return buildTaskPresentationCard(msg)
	}
	return buildGenericInteractiveCard(msg)
}

func buildGenericInteractiveCard(msg contracts.OutboundMessage) ([]byte, error) {
	elements := make([]any, 0, len(msg.Fields)+len(msg.Options)+3)
	if len(msg.Fields) > 0 {
		elements = append(elements, metadataGrid(msg.Fields))
	}
	if len(msg.Fields) > 0 && msg.BodyMarkdown != "" {
		elements = append(elements, map[string]any{"tag": "hr", "margin": "0px"})
	}
	if msg.BodyMarkdown != "" {
		elements = append(elements, map[string]any{
			"tag":        "markdown",
			"element_id": "card_body",
			"content":    msg.BodyMarkdown,
			"text_size":  "normal",
		})
	}
	if len(msg.Options) > 0 {
		elements = append(elements, optionRows(msg.Options)...)
	}

	elements = appendCardActions(elements, msg.Actions)
	return marshalCard(msg, elements)
}

func buildTaskPresentationCard(msg contracts.OutboundMessage) ([]byte, error) {
	presentation := *msg.Presentation
	elements := make([]any, 0, 8)
	switch presentation.Layout {
	case contracts.TaskCardResult:
		elements = append(elements, resultCardElements(msg, presentation)...)
	default:
		elements = append(elements, runningCardElements(msg, presentation)...)
	}
	elements = appendCardActions(elements, msg.Actions)
	return marshalCard(msg, elements)
}

func marshalCard(msg contracts.OutboundMessage, elements []any) ([]byte, error) {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   "fill",
			"summary":      map[string]any{"content": cardSummary(msg)},
		},
		"header": map[string]any{
			"template":      templateFor(msg),
			"title":         map[string]any{"tag": "plain_text", "content": msg.Title},
			"subtitle":      map[string]any{"tag": "plain_text", "content": cardSubtitle(msg)},
			"text_tag_list": []any{statusTag(msg)},
			"icon":          headerIcon(msg),
			"padding":       "12px",
		},
		"body": map[string]any{
			"direction":        "vertical",
			"padding":          "12px",
			"vertical_spacing": "12px",
			"elements":         elements,
		},
	}
	return json.Marshal(card)
}

func runningCardElements(msg contracts.OutboundMessage, presentation contracts.TaskPresentation) []any {
	stage := presentation.Stage
	if stage == "" {
		stage = "执行中"
	}
	activity := presentation.Activity
	if activity == "" {
		activity = "Codex 正在处理。"
	}
	elements := []any{metadataGrid([]contracts.Field{
		{Title: "阶段", Value: stage},
		{Title: "里程碑", Value: fmt.Sprintf("%d 已完成", len(presentation.Milestones))},
		{Title: "状态", Value: statusLabel(msg)},
	})}
	elements = append(elements, sectionMarkdown("当前活动", activity))
	if len(presentation.UserInputs) > 0 {
		elements = append(elements, sectionMarkdown("本轮输入", markdownStrings(presentation.UserInputs)))
	}
	if len(presentation.Milestones) > 0 {
		elements = append(elements, sectionMarkdown("关键里程碑", markdownList(presentation.Milestones)))
	}
	if presentation.ProcessingDetail != "" || msg.StreamDetail {
		elements = append(elements, sectionMarkdownWithElementID("处理详情", presentation.ProcessingDetail, taskProcessingDetailElementID))
	}
	return elements
}

func resultCardElements(msg contracts.OutboundMessage, presentation contracts.TaskPresentation) []any {
	conclusion := presentation.Conclusion
	if conclusion == "" {
		conclusion = "任务已完成。"
	}
	elements := []any{metadataGrid([]contracts.Field{
		{Title: "结果", Value: statusLabel(msg)},
		{Title: "改动", Value: fmt.Sprintf("%d 项", len(presentation.Changes))},
		{Title: "验证", Value: fmt.Sprintf("%d 项", len(presentation.Verification))},
	})}
	elements = append(elements, sectionMarkdown("结论", conclusion))
	if len(presentation.UserInputs) > 0 {
		elements = append(elements, sectionMarkdown("本轮输入", markdownStrings(presentation.UserInputs)))
	}
	if len(presentation.Changes) > 0 {
		elements = append(elements, sectionMarkdown("改动", markdownStrings(presentation.Changes)))
	}
	if len(presentation.Verification) > 0 {
		elements = append(elements, sectionMarkdown("验证", markdownStrings(presentation.Verification)))
	}
	return elements
}

func sectionMarkdown(title, content string) map[string]any {
	return sectionMarkdownWithElementID(title, content, "")
}

func sectionMarkdownWithElementID(title, content, elementID string) map[string]any {
	markdown := map[string]any{"tag": "markdown", "content": content, "text_size": "normal"}
	if elementID != "" {
		markdown["element_id"] = elementID
	}
	return map[string]any{
		"tag":       "column_set",
		"flex_mode": "stretch",
		"columns": []any{map[string]any{
			"tag":              "column",
			"width":            "weighted",
			"weight":           1,
			"vertical_spacing": "4px",
			"elements": []any{
				plainText(title, "notation", "grey"),
				markdown,
			},
		}},
	}
}

func markdownList(values []contracts.TaskMilestone) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		if value.Label != "" {
			items = append(items, value.Label)
		}
	}
	return markdownStrings(items)
}

func markdownStrings(values []string) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			items = append(items, "- "+value)
		}
	}
	return strings.Join(items, "\n")
}

func appendCardActions(elements []any, actions []contracts.Action) []any {
	var followUpAction *contracts.Action
	buttonActions := make([]contracts.Action, 0, len(actions))
	for _, action := range actions {
		if isFollowUpAction(action.ID) {
			actionCopy := action
			followUpAction = &actionCopy
			continue
		}
		buttonActions = append(buttonActions, action)
	}
	if len(buttonActions) > 0 {
		elements = append(elements, actionButtons(buttonActions))
	}
	if followUpAction != nil {
		if len(elements) > 0 {
			elements = append(elements, map[string]any{"tag": "hr", "margin": "4px 0px"})
		}
		elements = append(elements, followUpForm(*followUpAction))
	}
	return elements
}

func isFollowUpAction(actionID string) bool {
	return actionID == "continue_submit" || actionID == "steer_submit"
}

func followUpForm(action contracts.Action) map[string]any {
	submit := actionButton(action)
	submit["name"] = action.ID
	submit["form_action_type"] = "submit"
	submit["width"] = "default"
	label, placeholder := followUpCopy(action.ID)
	return map[string]any{
		"tag":              "form",
		"name":             "codex_followup_form",
		"vertical_spacing": "8px",
		"elements": []any{
			map[string]any{
				"tag":         "input",
				"name":        "text",
				"required":    true,
				"input_type":  "multiline_text",
				"rows":        2,
				"auto_resize": true,
				"max_rows":    6,
				"max_length":  1000,
				"width":       "fill",
				"label":       map[string]any{"tag": "plain_text", "content": label},
				"placeholder": map[string]any{"tag": "plain_text", "content": placeholder},
			},
			map[string]any{
				"tag":                "column_set",
				"flex_mode":          "flow",
				"horizontal_spacing": "small",
				"columns": []any{map[string]any{
					"tag":            "column",
					"width":          "auto",
					"vertical_align": "center",
					"elements":       []any{submit},
				}},
			},
		},
	}
}

func followUpCopy(actionID string) (string, string) {
	if actionID == "steer_submit" {
		return "补充到本轮", "补充目标、约束或调整方向"
	}
	return "继续跟进", "继续补充需求或问题"
}

func metadataGrid(fields []contracts.Field) map[string]any {
	columns := make([]any, 0, len(fields))
	for _, field := range fields {
		columns = append(columns, map[string]any{
			"tag":              "column",
			"width":            "weighted",
			"weight":           1,
			"background_style": "grey",
			"padding":          "8px",
			"vertical_spacing": "4px",
			"elements": []any{
				plainText(field.Title, "notation", "grey"),
				plainText(field.Value, "normal", "default"),
			},
		})
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "stretch",
		"horizontal_spacing": "small",
		"columns":            columns,
	}
}

func optionRows(options []contracts.CardOption) []any {
	elements := make([]any, 0, len(options))
	for index, option := range options {
		content := []any{plainText(option.Title, "heading", "default")}
		if option.Detail != "" {
			content = append(content, plainText(option.Detail, "normal", "default"))
		}
		if option.Meta != "" {
			content = append(content, plainText(option.Meta, "notation", "grey"))
		}
		elements = append(elements, map[string]any{
			"tag":              "interactive_container",
			"element_id":       fmt.Sprintf("option_%d", index),
			"width":            "fill",
			"height":           "auto",
			"background_style": "default",
			"has_border":       true,
			"border_color":     "grey",
			"corner_radius":    "8px",
			"padding":          "10px 12px",
			"vertical_spacing": "4px",
			"hover_tips":       plainTextContent("接管此会话"),
			"behaviors":        []any{callbackBehavior(option.Action)},
			"elements": []any{map[string]any{
				"tag":                "column_set",
				"flex_mode":          "stretch",
				"horizontal_spacing": "medium",
				"columns": []any{
					map[string]any{
						"tag":              "column",
						"width":            "weighted",
						"weight":           1,
						"vertical_spacing": "4px",
						"elements":         content,
					},
					map[string]any{
						"tag":            "column",
						"width":          "auto",
						"vertical_align": "center",
						"elements":       []any{actionButton(option.Action)},
					},
				},
			}},
		})
	}
	return elements
}

func actionButtons(actions []contracts.Action) map[string]any {
	columns := make([]any, 0, len(actions))
	for _, action := range actions {
		columns = append(columns, map[string]any{
			"tag":            "column",
			"width":          "auto",
			"vertical_align": "center",
			"elements":       []any{actionButton(action)},
		})
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "flow",
		"horizontal_spacing": "small",
		"columns":            columns,
	}
}

func actionButton(action contracts.Action) map[string]any {
	button := map[string]any{
		"tag":       "button",
		"type":      buttonType(action.Style),
		"size":      "medium",
		"text":      map[string]any{"tag": "plain_text", "content": action.Label},
		"behaviors": []any{callbackBehavior(action)},
	}
	if action.ID == "stop_task" {
		button["hover_tips"] = plainTextContent("停止当前任务")
		button["confirm"] = map[string]any{
			"title": plainTextContent("停止当前任务？"),
			"text":  plainTextContent("Codex 将收到停止请求，当前未完成的工作可能会中断。"),
		}
	}
	return button
}

func plainText(content, size, color string) map[string]any {
	return map[string]any{
		"tag": "div",
		"text": map[string]any{
			"tag":        "plain_text",
			"content":    content,
			"text_size":  size,
			"text_color": color,
		},
	}
}

func plainTextContent(content string) map[string]any {
	return map[string]any{"tag": "plain_text", "content": content}
}

func templateFor(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "green"
	case contracts.CardRestarting:
		return "orange"
	case contracts.CardFailure:
		if msg.Status == "canceled" {
			return "grey"
		}
		return "red"
	case contracts.CardRoutingError:
		return "red"
	case contracts.CardRunningConflict:
		return "orange"
	case contracts.CardThreadSelection:
		return "blue"
	default:
		switch msg.Status {
		case "idle", "succeeded":
			return "green"
		case "queued":
			return "orange"
		case "canceled":
			return "grey"
		case "failed":
			return "red"
		default:
			return "wathet"
		}
	}
}

func cardSummary(msg contracts.OutboundMessage) string {
	if msg.Title != "" {
		if msg.Subtitle != "" {
			return msg.Title + " · " + msg.Subtitle
		}
		return msg.Title
	}
	return statusLabel(msg)
}

func cardSubtitle(msg contracts.OutboundMessage) string {
	if msg.Subtitle != "" {
		return msg.Subtitle
	}
	if msg.Status == "canceled" {
		return "本机 Codex 已停止执行"
	}
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "本机 Codex 已完成执行"
	case contracts.CardRestarting:
		return "正在重新连接 Feishu 与 Codex"
	case contracts.CardFailure:
		return "本机 Codex 需要你的处理"
	case contracts.CardThreadSelection:
		return "从桌面 Codex 会话继续"
	case contracts.CardRunningConflict:
		return "当前任务仍在运行"
	case contracts.CardRoutingError:
		return "请从有效任务卡片继续"
	default:
		return "本机 Codex 远程任务"
	}
}

func headerIcon(msg contracts.OutboundMessage) map[string]any {
	token, color := "chat_outlined", "blue"
	switch msg.CardKind {
	case contracts.CardSuccess:
		token, color = "thumbsup_outlined", "green"
	case contracts.CardRestarting:
		token, color = "chat_outlined", "orange"
	case contracts.CardFailure:
		if msg.Status == "canceled" {
			token, color = "chat_outlined", "grey"
			break
		}
		token, color = "chat-forbidden_outlined", "red"
	case contracts.CardRoutingError:
		token, color = "chat-forbidden_outlined", "red"
	case contracts.CardThreadSelection:
		token, color = "chat-history_outlined", "blue"
	case contracts.CardRunningConflict:
		token, color = "chat_outlined", "orange"
	default:
		switch msg.Status {
		case "idle", "succeeded":
			color = "green"
		case "queued":
			color = "orange"
		case "canceled":
			color = "grey"
		case "failed":
			color = "red"
		}
	}
	return map[string]any{
		"tag":   "standard_icon",
		"token": token,
		"color": color,
	}
}

func statusTag(msg contracts.OutboundMessage) map[string]any {
	return map[string]any{
		"tag":   "text_tag",
		"text":  map[string]any{"tag": "plain_text", "content": statusLabel(msg)},
		"color": statusColor(msg),
	}
}

func statusLabel(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "已完成"
	case contracts.CardFailure:
		if msg.Status == "canceled" {
			return "已停止"
		}
		return "未完成"
	case contracts.CardRoutingError:
		return "待处理"
	case contracts.CardThreadSelection:
		return "选择会话"
	case contracts.CardRestarting:
		return "重启中"
	case contracts.CardRunningConflict:
		return "运行中"
	}
	switch msg.Status {
	case "idle":
		return "已接管"
	case "queued":
		return "排队中"
	case "running":
		return "运行中"
	case "succeeded":
		return "已完成"
	case "failed":
		return "未完成"
	case "canceled":
		return "已停止"
	default:
		return "Codex"
	}
}

func statusColor(msg contracts.OutboundMessage) string {
	switch msg.CardKind {
	case contracts.CardSuccess:
		return "green"
	case contracts.CardFailure:
		if msg.Status == "canceled" {
			return "grey"
		}
		return "red"
	case contracts.CardRoutingError:
		return "red"
	case contracts.CardRunningConflict:
		return "orange"
	case contracts.CardThreadSelection:
		return "blue"
	case contracts.CardRestarting:
		return "orange"
	}
	switch msg.Status {
	case "idle", "succeeded":
		return "green"
	case "queued":
		return "orange"
	case "running":
		return "wathet"
	case "failed":
		return "red"
	case "canceled":
		return "grey"
	default:
		return "neutral"
	}
}

func callbackBehavior(action contracts.Action) map[string]any {
	return map[string]any{"type": "callback", "value": actionValue(action)}
}

func actionValue(action contracts.Action) map[string]string {
	value := make(map[string]string, len(action.Value)+1)
	for key, val := range action.Value {
		value[key] = val
	}
	value["action_id"] = action.ID
	return value
}

func buttonType(style string) string {
	if style == "primary" {
		return "primary_filled"
	}
	if style == "" {
		return "default"
	}
	return style
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
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
