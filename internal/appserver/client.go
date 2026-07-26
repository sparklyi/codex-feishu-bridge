package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

var ErrClosed = errors.New("app-server client closed")

const (
	fullAccessSandbox = "danger-full-access"
	approvalPolicy    = "never"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Client owns one initialized JSON-RPC connection to Codex app-server.
type Client struct {
	reader io.ReadCloser
	writer io.WriteCloser
	close  func() error

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpcMessage
	nextID  atomic.Int64

	events        chan Event
	requests      chan ServerRequest
	done          chan struct{}
	readDone      chan struct{}
	closeOnce     sync.Once
	errMu         sync.Mutex
	err           error
	shutdownError error
}

func NewClient(reader io.ReadCloser, writer io.WriteCloser, closeFn func() error) *Client {
	c := &Client{
		reader:   reader,
		writer:   writer,
		close:    closeFn,
		pending:  make(map[string]chan rpcMessage),
		events:   make(chan Event, 512),
		requests: make(chan ServerRequest, 64),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Client) Initialize(ctx context.Context, name, version string) error {
	if name == "" {
		name = "codex-feishu-bridge"
	}
	if version == "" {
		version = "dev"
	}
	var ignored json.RawMessage
	if err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    name,
			"title":   "Codex Feishu Bridge",
			"version": version,
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &ignored); err != nil {
		return err
	}
	return c.Notify("initialized", map[string]any{})
}

func (c *Client) Events() <-chan Event {
	return c.events
}

func (c *Client) Requests() <-chan ServerRequest {
	return c.requests
}

func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	response := make(chan rpcMessage, 1)
	c.mu.Lock()
	if c.closedLocked() {
		c.mu.Unlock()
		return c.closeErr()
	}
	c.pending[key] = response
	c.mu.Unlock()

	if err := c.writeMessage(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.removePending(key)
		return err
	}

	select {
	case msg := <-response:
		if msg.Error != nil {
			return msg.Error
		}
		if result == nil || len(msg.Result) == 0 || string(msg.Result) == "null" {
			return nil
		}
		if err := json.Unmarshal(msg.Result, result); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(key)
		return ctx.Err()
	case <-c.done:
		return c.closeErr()
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.writeMessage(map[string]any{"method": method, "params": params})
}

func (c *Client) Respond(ctx context.Context, id json.RawMessage, result any) error {
	if len(id) == 0 {
		return errors.New("app-server request id is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	var decoded any
	if err := json.Unmarshal(id, &decoded); err != nil {
		return fmt.Errorf("decode app-server request id: %w", err)
	}
	return c.writeMessage(map[string]any{"id": decoded, "result": result})
}

func (c *Client) ListThreads(ctx context.Context, limit int) ([]Thread, error) {
	if limit <= 0 {
		limit = 12
	}
	var out struct {
		Data []Thread `json:"data"`
	}
	err := c.Call(ctx, "thread/list", map[string]any{
		"limit":         limit,
		"sortKey":       "recency_at",
		"sortDirection": "desc",
	}, &out)
	return out.Data, err
}

func (c *Client) StartThread(ctx context.Context, in ThreadStartInput) (Thread, error) {
	params := map[string]any{
		"cwd":            in.CWD,
		"sandbox":        fullAccessSandbox,
		"approvalPolicy": approvalPolicy,
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	var out struct {
		Thread Thread `json:"thread"`
	}
	err := c.Call(ctx, "thread/start", params, &out)
	return out.Thread, err
}

func (c *Client) ResumeThread(ctx context.Context, in ThreadResumeInput) (Thread, error) {
	if in.ThreadID == "" {
		return Thread{}, errors.New("thread id is required")
	}
	params := map[string]any{
		"threadId":       in.ThreadID,
		"sandbox":        fullAccessSandbox,
		"approvalPolicy": approvalPolicy,
	}
	if in.CWD != "" {
		params["cwd"] = in.CWD
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	var out struct {
		Thread Thread `json:"thread"`
	}
	err := c.Call(ctx, "thread/resume", params, &out)
	return out.Thread, err
}

func (c *Client) StartTurn(ctx context.Context, in TurnStartInput) (Turn, error) {
	if in.ThreadID == "" {
		return Turn{}, errors.New("thread id is required")
	}
	params := map[string]any{
		"threadId":       in.ThreadID,
		"input":          []map[string]string{{"type": "text", "text": in.Text}},
		"sandboxPolicy":  fullAccessSandboxPolicy(),
		"approvalPolicy": approvalPolicy,
	}
	if in.CWD != "" {
		params["cwd"] = in.CWD
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	var out struct {
		Turn Turn `json:"turn"`
	}
	err := c.Call(ctx, "turn/start", params, &out)
	return out.Turn, err
}

func (c *Client) SteerTurn(ctx context.Context, in TurnSteerInput) (string, error) {
	if in.ThreadID == "" || in.ExpectedTurnID == "" {
		return "", errors.New("thread id and expected turn id are required")
	}
	if in.Text == "" {
		return "", errors.New("steer text is required")
	}
	var out struct {
		TurnID string `json:"turnId"`
	}
	err := c.Call(ctx, "turn/steer", map[string]any{
		"threadId":       in.ThreadID,
		"expectedTurnId": in.ExpectedTurnID,
		"input":          []map[string]string{{"type": "text", "text": in.Text}},
	}, &out)
	return out.TurnID, err
}

func (c *Client) Interrupt(ctx context.Context, threadID, turnID string) error {
	if threadID == "" || turnID == "" {
		return errors.New("thread id and turn id are required")
	}
	return c.Call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, nil)
}

func fullAccessSandboxPolicy() map[string]any {
	return map[string]any{"type": "dangerFullAccess"}
}

func (c *Client) Close() error {
	c.shutdown(ErrClosed)
	<-c.readDone
	return c.shutdownErr()
}

func (c *Client) readLoop() {
	defer close(c.events)
	defer close(c.requests)
	defer close(c.readDone)
	scanner := bufio.NewScanner(c.reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case len(msg.ID) > 0 && msg.Method == "":
			c.deliverResponse(msg)
		case len(msg.ID) > 0 && msg.Method != "":
			if !c.publishRequest(ServerRequest{ID: msg.ID, Method: msg.Method, Params: msg.Params}) {
				return
			}
		case msg.Method != "":
			if !c.publishEvent(Event{Method: msg.Method, Params: msg.Params}) {
				return
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.shutdown(err)
}

func (c *Client) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.setCloseErr(err)
		close(c.done)
		if c.writer != nil {
			if closeErr := c.writer.Close(); closeErr != nil {
				c.setShutdownErr(closeErr)
			}
		}
		if c.reader != nil {
			if closeErr := c.reader.Close(); closeErr != nil {
				c.setShutdownErr(closeErr)
			}
		}
		if c.close != nil {
			if closeErr := c.close(); closeErr != nil {
				c.setShutdownErr(closeErr)
			}
		}
		c.failPending(c.closeErr())
	})
}

func (c *Client) writeMessage(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	select {
	case <-c.done:
		c.writeMu.Unlock()
		return c.closeErr()
	default:
	}
	_, err = c.writer.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err == nil {
		return nil
	}
	c.shutdown(err)
	return err
}

func (c *Client) deliverResponse(msg rpcMessage) {
	key := idKey(msg.ID)
	if key == "" {
		return
	}
	c.mu.Lock()
	ch := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (c *Client) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan rpcMessage)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcMessage{Error: &RPCError{Code: -1, Message: err.Error()}}
	}
}

func (c *Client) publishEvent(event Event) bool {
	select {
	case <-c.done:
		return false
	case c.events <- event:
		return true
	}
}

func (c *Client) publishRequest(request ServerRequest) bool {
	select {
	case <-c.done:
		return false
	case c.requests <- request:
		return true
	}
}

func (c *Client) closedLocked() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Client) setCloseErr(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

func (c *Client) setShutdownErr(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	if c.shutdownError == nil {
		c.shutdownError = err
	}
	c.errMu.Unlock()
}

func (c *Client) shutdownErr() error {
	c.errMu.Lock()
	err := c.shutdownError
	c.errMu.Unlock()
	return err
}

func (c *Client) closeErr() error {
	c.errMu.Lock()
	err := c.err
	c.errMu.Unlock()
	if err != nil {
		return err
	}
	return ErrClosed
}

func idKey(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}
