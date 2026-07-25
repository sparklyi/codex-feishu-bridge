package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func TestFeishuWSClientBootstrapReadsEndpoint(t *testing.T) {
	var received larkws.BootstrapRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback/ws/endpoint" {
			t.Fatalf("unexpected bootstrap path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(larkws.EndpointResp{
			Code: larkws.OK,
			Data: &larkws.Endpoint{Url: "wss://example.test/ws?service_id=73"},
		})
	}))
	defer server.Close()

	client := newFeishuWSClient("cli_test", "secret", nil)
	client.httpClient = server.Client()
	client.endpointURL = server.URL + "/callback/ws/endpoint"
	endpoint, serviceID, err := client.bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "wss://example.test/ws?service_id=73" || serviceID != 73 {
		t.Fatalf("unexpected endpoint result: endpoint=%q service_id=%d", endpoint, serviceID)
	}
	if received.AppID != "cli_test" || received.AppSecret != "secret" || received.ClientAssertion != "" {
		t.Fatalf("unexpected bootstrap request: %+v", received)
	}
}

func TestFeishuTransportsBypassProxyEnvironment(t *testing.T) {
	httpClient := newFeishuHTTPClient(time.Second)
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", httpClient.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("Feishu HTTP client must not use an environment proxy")
	}
	if dialer := newFeishuWebSocketDialer(); dialer.Proxy != nil {
		t.Fatal("Feishu WebSocket dialer must not use an environment proxy")
	}
}

func TestFeishuWSClientSendsProtocolHeartbeat(t *testing.T) {
	upgrader := websocket.Upgrader{}
	frames := make(chan larkws.Frame, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			var frame larkws.Frame
			if err := frame.Unmarshal(payload); err != nil {
				t.Error(err)
				return
			}
			frames <- frame
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &feishuWSClient{heartbeatInterval: 20 * time.Millisecond, writeTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.serveConnection(ctx, conn, 73) }()

	for i := 0; i < 2; i++ {
		select {
		case frame := <-frames:
			headers := larkws.Headers(frame.Headers)
			if larkws.FrameType(frame.Method) != larkws.FrameTypeControl || larkws.MessageType(headers.GetString(larkws.HeaderType)) != larkws.MessageTypePing || frame.Service != 73 {
				t.Fatalf("unexpected heartbeat frame: %+v", frame)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for protocol heartbeat")
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serveConnection error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveConnection did not stop after cancellation")
	}
}

func TestFeishuWSClientDispatchesEventAndReplies(t *testing.T) {
	upgrader := websocket.Upgrader{}
	responseFrames := make(chan larkws.Frame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()

		payload, err := json.Marshal(map[string]any{
			"schema": "2.0",
			"header": map[string]any{
				"event_id":    "evt_1",
				"event_type":  "im.message.receive_v1",
				"app_id":      "cli_test",
				"create_time": "1760000000000",
			},
			"event": map[string]any{
				"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_owner"}},
				"message": map[string]any{
					"message_id":   "msg_1",
					"chat_id":      "chat_1",
					"chat_type":    "private",
					"message_type": "text",
					"content":      `{"text":"run the test"}`,
				},
			},
		})
		if err != nil {
			t.Error(err)
			return
		}
		headers := larkws.Headers{}
		headers.Add(larkws.HeaderType, string(larkws.MessageTypeEvent))
		headers.Add(larkws.HeaderMessageID, "event_1")
		frame := &larkws.Frame{Method: int32(larkws.FrameTypeData), Service: 73, Headers: []larkws.Header(headers), Payload: payload}
		encoded, err := frame.Marshal()
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, encoded); err != nil {
			t.Error(err)
			return
		}

		for {
			_, response, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var received larkws.Frame
			if received.Unmarshal(response) != nil || larkws.FrameType(received.Method) != larkws.FrameTypeData {
				continue
			}
			responseFrames <- received
			return
		}
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	source := NewSDKEventSource("cli_test", "secret", "")
	source.client.heartbeatInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- source.client.serveConnection(ctx, conn, 73) }()

	select {
	case frame := <-responseFrames:
		var response larkws.Response
		if err := json.Unmarshal(frame.Payload, &response); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("response code = %d, want %d", response.StatusCode, http.StatusOK)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event response")
	}
	select {
	case event := <-source.events:
		if event.err != nil || event.raw.Kind != RawEventMessage {
			t.Fatalf("unexpected dispatched event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dispatched event")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveConnection did not stop")
	}
}

func TestFeishuWSClientAcknowledgesCardActions(t *testing.T) {
	upgrader := websocket.Upgrader{}
	responseFrames := make(chan larkws.Frame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()

		payload, err := json.Marshal(map[string]any{
			"schema": "2.0",
			"header": map[string]any{
				"event_id":    "evt_card_1",
				"event_type":  "card.action.trigger",
				"create_time": "1760000000000",
			},
			"event": map[string]any{
				"operator": map[string]any{"open_id": "ou_owner"},
				"context":  map[string]any{"open_message_id": "card_1", "open_chat_id": "chat_1"},
				"action": map[string]any{
					"name":  "stop_task",
					"tag":   "button",
					"value": map[string]any{"action_id": "stop_task", "action": "stop_task", "task_id": "task_1"},
				},
			},
		})
		if err != nil {
			t.Error(err)
			return
		}
		headers := larkws.Headers{}
		headers.Add(larkws.HeaderType, string(larkws.MessageTypeEvent))
		headers.Add(larkws.HeaderMessageID, "card_event_1")
		frame := &larkws.Frame{Method: int32(larkws.FrameTypeData), Service: 73, Headers: []larkws.Header(headers), Payload: payload}
		encoded, err := frame.Marshal()
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, encoded); err != nil {
			t.Error(err)
			return
		}
		for {
			_, response, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var received larkws.Frame
			if received.Unmarshal(response) != nil || larkws.FrameType(received.Method) != larkws.FrameTypeData {
				continue
			}
			responseFrames <- received
			return
		}
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	source := NewSDKEventSource("cli_test", "secret", "")
	source.client.heartbeatInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- source.client.serveConnection(ctx, conn, 73) }()

	select {
	case frame := <-responseFrames:
		var response larkws.Response
		if err := json.Unmarshal(frame.Payload, &response); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("response code = %d, want %d", response.StatusCode, http.StatusOK)
		}
		var callbackResponse callback.CardActionTriggerResponse
		if err := json.Unmarshal(response.Data, &callbackResponse); err != nil {
			t.Fatal(err)
		}
		if callbackResponse.Toast == nil || callbackResponse.Toast.Type != "success" || callbackResponse.Toast.Content == "" {
			t.Fatalf("missing callback acknowledgement: %+v", callbackResponse)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for card action response")
	}
	select {
	case event := <-source.cardActions:
		if event.err != nil || event.raw.Kind != RawEventCardAction {
			t.Fatalf("unexpected card action: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for card action")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveConnection did not stop")
	}
}
