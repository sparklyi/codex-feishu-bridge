package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const (
	feishuWSEndpoint        = "https://open.feishu.cn/callback/ws/endpoint"
	feishuHTTPTimeout       = 30 * time.Second
	feishuBootstrapTimeout  = 8 * time.Second
	feishuHeartbeatInterval = 30 * time.Second
	maxHeartbeatInterval    = 30 * time.Second
	feishuReconnectDelay    = time.Second
	feishuWriteTimeout      = 5 * time.Second
	fragmentTTL             = 5 * time.Second
)

// feishuWSClient owns the parts of the Feishu long connection that the SDK
// leaves server-configurable. It keeps a conservative protocol heartbeat and
// reconnects on a direct network path when the remote endpoint closes the
// connection.
type feishuWSClient struct {
	appID      string
	appSecret  string
	dispatcher *dispatcher.EventDispatcher

	httpClient  *http.Client
	dialer      *websocket.Dialer
	endpointURL string

	bootstrapTimeout time.Duration
	// heartbeatInterval is an explicit local override used by tests. Production
	// connections use the value Feishu supplies during WebSocket bootstrap.
	heartbeatInterval       time.Duration
	serverHeartbeatInterval time.Duration
	reconnectDelay          time.Duration
	writeTimeout            time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	active *websocket.Conn
	closed bool

	fragmentsMu sync.Mutex
	fragments   map[string]fragmentBuffer
}

type fragmentBuffer struct {
	frame     larkws.Frame
	parts     [][]byte
	received  int
	expiresAt time.Time
}

func newFeishuWSClient(appID, appSecret string, eventDispatcher *dispatcher.EventDispatcher, proxyURL *url.URL) *feishuWSClient {
	return &feishuWSClient{
		appID:            appID,
		appSecret:        appSecret,
		dispatcher:       eventDispatcher,
		httpClient:       newFeishuHTTPClient(feishuHTTPTimeout, proxyURL),
		dialer:           newFeishuWebSocketDialer(proxyURL),
		endpointURL:      feishuWSEndpoint,
		bootstrapTimeout: feishuBootstrapTimeout,
		reconnectDelay:   feishuReconnectDelay,
		writeTimeout:     feishuWriteTimeout,
		fragments:        make(map[string]fragmentBuffer),
	}
}

func newFeishuHTTPClient(timeout time.Duration, proxyURL *url.URL) *http.Client {
	transport := newFeishuTransport()
	transport.Proxy = staticProxy(proxyURL)
	return &http.Client{Transport: transport, Timeout: timeout}
}

func staticProxy(proxyURL *url.URL) func(*http.Request) (*url.URL, error) {
	if proxyURL == nil {
		return nil
	}
	proxyCopy := *proxyURL
	return http.ProxyURL(&proxyCopy)
}

func newFeishuTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 30 * time.Second
	return transport
}

func newFeishuWebSocketDialer(proxyURL *url.URL) *websocket.Dialer {
	dialer := &net.Dialer{Timeout: feishuBootstrapTimeout, KeepAlive: 20 * time.Second}
	return &websocket.Dialer{
		HandshakeTimeout: feishuBootstrapTimeout,
		NetDialContext:   dialer.DialContext,
		Proxy:            staticProxy(proxyURL),
	}
}

func (c *feishuWSClient) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return context.Canceled
	}
	c.cancel = cancel
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.cancel = nil
		c.active = nil
		c.mu.Unlock()
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, serviceID, err := c.connect(ctx)
		if err != nil {
			slog.Warn("Feishu WebSocket connection attempt failed", "error", safeNetworkError("connection", err))
			if err := sleepContext(ctx, c.reconnectDelay); err != nil {
				return err
			}
			continue
		}
		if !c.setActive(conn) {
			_ = conn.Close()
			return context.Canceled
		}

		slog.Info("Feishu WebSocket connected")
		err = c.serveConnection(ctx, conn, serviceID)
		c.clearActive(conn)
		_ = conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("Feishu WebSocket disconnected", "error", err)
		}
		if err := sleepContext(ctx, c.reconnectDelay); err != nil {
			return err
		}
	}
}

func (c *feishuWSClient) Close() error {
	c.mu.Lock()
	c.closed = true
	cancel := c.cancel
	conn := c.active
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (c *feishuWSClient) setActive(conn *websocket.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.active = conn
	return true
}

func (c *feishuWSClient) clearActive(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == conn {
		c.active = nil
	}
}

func (c *feishuWSClient) connect(ctx context.Context) (*websocket.Conn, int32, error) {
	bootstrapCtx, cancelBootstrap := context.WithTimeout(ctx, c.bootstrapTimeout)
	endpoint, serviceID, heartbeatInterval, err := c.bootstrap(bootstrapCtx)
	cancelBootstrap()
	if err != nil {
		return nil, 0, err
	}
	c.setServerHeartbeatInterval(heartbeatInterval)
	dialCtx, cancelDial := context.WithTimeout(ctx, c.bootstrapTimeout)
	defer cancelDial()
	conn, response, err := c.dialer.DialContext(dialCtx, endpoint, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, 0, err
	}
	return conn, serviceID, nil
}

func (c *feishuWSClient) bootstrap(ctx context.Context) (string, int32, time.Duration, error) {
	body, err := json.Marshal(larkws.BootstrapRequest{AppID: c.appID, AppSecret: c.appSecret})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "zh")
	req.Header.Set("User-Agent", "codex-feishu-bridge")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("bootstrap returned HTTP %d", resp.StatusCode)
	}

	endpoint := &larkws.EndpointResp{}
	if err := json.Unmarshal(payload, endpoint); err != nil {
		return "", 0, 0, err
	}
	if endpoint.Code != larkws.OK {
		return "", 0, 0, fmt.Errorf("bootstrap returned code %d", endpoint.Code)
	}
	if endpoint.Data == nil || endpoint.Data.Url == "" {
		return "", 0, 0, errors.New("bootstrap returned no WebSocket endpoint")
	}
	serviceID, err := endpointServiceID(endpoint.Data.Url)
	if err != nil {
		return "", 0, 0, err
	}
	return endpoint.Data.Url, serviceID, heartbeatFromConfig(endpoint.Data.ClientConfig), nil
}

func heartbeatFromConfig(config *larkws.ClientConfig) time.Duration {
	if config == nil || config.PingInterval <= 0 {
		return feishuHeartbeatInterval
	}
	interval := time.Duration(config.PingInterval) * time.Second / 2
	if interval <= 0 {
		return feishuHeartbeatInterval
	}
	if interval > maxHeartbeatInterval {
		return maxHeartbeatInterval
	}
	return interval
}

func (c *feishuWSClient) setServerHeartbeatInterval(interval time.Duration) {
	if interval <= 0 {
		interval = feishuHeartbeatInterval
	}
	c.mu.Lock()
	c.serverHeartbeatInterval = interval
	c.mu.Unlock()
}

func (c *feishuWSClient) currentHeartbeatInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.heartbeatInterval > 0 {
		return c.heartbeatInterval
	}
	if c.serverHeartbeatInterval > 0 {
		return c.serverHeartbeatInterval
	}
	return feishuHeartbeatInterval
}

func endpointServiceID(rawURL string) (int32, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return 0, errors.New("invalid WebSocket endpoint")
	}
	serviceID, err := strconv.ParseInt(parsed.Query().Get(larkws.ServiceID), 10, 32)
	if err != nil || serviceID == 0 {
		return 0, errors.New("WebSocket endpoint has no service id")
	}
	return int32(serviceID), nil
}

func (c *feishuWSClient) serveConnection(ctx context.Context, conn *websocket.Conn, serviceID int32) error {
	stop := make(chan struct{})
	var workers sync.WaitGroup
	var writeMu sync.Mutex
	defer workers.Wait()
	defer close(stop)

	if err := c.writePing(conn, serviceID, &writeMu); err != nil {
		return err
	}
	workers.Add(2)
	go func() {
		defer workers.Done()
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	go c.heartbeatLoop(ctx, stop, conn, serviceID, &writeMu, &workers)

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		var frame larkws.Frame
		if err := frame.Unmarshal(payload); err != nil {
			slog.Warn("Feishu WebSocket frame rejected", "error", err)
			continue
		}
		frame, complete := c.completeFrame(frame)
		if !complete {
			continue
		}
		if larkws.FrameType(frame.Method) == larkws.FrameTypeControl {
			c.handleControlFrame(frame)
			continue
		}
		if larkws.FrameType(frame.Method) != larkws.FrameTypeData {
			continue
		}
		c.handleDataFrame(ctx, conn, frame, &writeMu)
	}
}

func (c *feishuWSClient) heartbeatLoop(ctx context.Context, stop <-chan struct{}, conn *websocket.Conn, serviceID int32, writeMu *sync.Mutex, workers *sync.WaitGroup) {
	defer workers.Done()
	timer := time.NewTimer(c.currentHeartbeatInterval())
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := c.writePing(conn, serviceID, writeMu); err != nil {
				_ = conn.Close()
				return
			}
			timer.Reset(c.currentHeartbeatInterval())
		}
	}
}

func (c *feishuWSClient) writePing(conn *websocket.Conn, serviceID int32, writeMu *sync.Mutex) error {
	frame := larkws.NewPingFrame(serviceID)
	return c.writeFrame(conn, frame, writeMu)
}

func (c *feishuWSClient) handleControlFrame(frame larkws.Frame) {
	headers := larkws.Headers(frame.Headers)
	if larkws.MessageType(headers.GetString(larkws.HeaderType)) != larkws.MessageTypePong || len(frame.Payload) == 0 {
		return
	}
	var config larkws.ClientConfig
	if err := json.Unmarshal(frame.Payload, &config); err != nil {
		slog.Warn("Feishu WebSocket pong config rejected", "error", err)
		return
	}
	c.setServerHeartbeatInterval(heartbeatFromConfig(&config))
}

func (c *feishuWSClient) handleDataFrame(ctx context.Context, conn *websocket.Conn, frame larkws.Frame, writeMu *sync.Mutex) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("Feishu WebSocket event handler panicked", "error", recovered)
		}
	}()
	headers := larkws.Headers(frame.Headers)
	if larkws.MessageType(headers.GetString(larkws.HeaderType)) != larkws.MessageTypeEvent || c.dispatcher == nil {
		return
	}

	started := time.Now()
	callbackResponse, handleErr := c.dispatcher.Do(ctx, frame.Payload)
	response := larkws.NewResponseByCode(http.StatusOK)
	if handleErr != nil {
		slog.Warn("Feishu WebSocket event handling failed", "error", handleErr)
		response = larkws.NewResponseByCode(http.StatusInternalServerError)
	} else if callbackResponse != nil {
		data, err := json.Marshal(callbackResponse)
		if err != nil {
			response = larkws.NewResponseByCode(http.StatusInternalServerError)
		} else {
			response.Data = data
		}
	}
	payload, err := json.Marshal(response)
	if err != nil {
		slog.Error("Feishu WebSocket response encoding failed", "error", err)
		return
	}
	headers.Add(larkws.HeaderBizRt, strconv.FormatInt(time.Since(started).Milliseconds(), 10))
	frame.Payload = payload
	frame.Headers = []larkws.Header(headers)
	if err := c.writeFrame(conn, &frame, writeMu); err != nil && ctx.Err() == nil {
		slog.Warn("Feishu WebSocket event response failed", "error", err)
	}
}

func (c *feishuWSClient) writeFrame(conn *websocket.Conn, frame *larkws.Frame, writeMu *sync.Mutex) error {
	payload, err := frame.Marshal()
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if c.writeTimeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			return err
		}
		defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	}
	return conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *feishuWSClient) completeFrame(frame larkws.Frame) (larkws.Frame, bool) {
	headers := larkws.Headers(frame.Headers)
	parts := headers.GetInt(larkws.HeaderSum)
	if parts <= 1 {
		return frame, true
	}
	part := headers.GetInt(larkws.HeaderSeq)
	messageID := headers.GetString(larkws.HeaderMessageID)
	if messageID == "" || part < 0 || part >= parts {
		return larkws.Frame{}, false
	}

	now := time.Now()
	c.fragmentsMu.Lock()
	defer c.fragmentsMu.Unlock()
	for key, pending := range c.fragments {
		if !pending.expiresAt.After(now) {
			delete(c.fragments, key)
		}
	}
	pending, ok := c.fragments[messageID]
	if !ok || len(pending.parts) != parts {
		pending = fragmentBuffer{frame: frame, parts: make([][]byte, parts), expiresAt: now.Add(fragmentTTL)}
	}
	if pending.parts[part] == nil {
		pending.parts[part] = append([]byte(nil), frame.Payload...)
		pending.received++
	}
	if pending.received < parts {
		c.fragments[messageID] = pending
		return larkws.Frame{}, false
	}

	length := 0
	for _, payload := range pending.parts {
		length += len(payload)
	}
	combined := make([]byte, 0, length)
	for _, payload := range pending.parts {
		combined = append(combined, payload...)
	}
	delete(c.fragments, messageID)
	pending.frame.Payload = combined
	return pending.frame, true
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		delay = feishuReconnectDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func safeNetworkError(operation string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out", operation)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%s timed out", operation)
	}
	return fmt.Errorf("%s failed", operation)
}
