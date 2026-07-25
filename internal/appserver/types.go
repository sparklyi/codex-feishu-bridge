package appserver

import (
	"context"
	"encoding/json"
	"fmt"
)

// API is the narrow app-server surface consumed by the bridge runtime. Keeping
// it here lets production use Client while tests can use a deterministic fake.
type API interface {
	ListThreads(ctx context.Context, limit int) ([]Thread, error)
	StartThread(ctx context.Context, in ThreadStartInput) (Thread, error)
	ResumeThread(ctx context.Context, in ThreadResumeInput) (Thread, error)
	StartTurn(ctx context.Context, in TurnStartInput) (Turn, error)
	Interrupt(ctx context.Context, threadID, turnID string) error
	Respond(ctx context.Context, id json.RawMessage, result any) error
	Events() <-chan Event
	Requests() <-chan ServerRequest
	Close() error
}

type Thread struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Preview   string       `json:"preview"`
	CWD       string       `json:"cwd"`
	Source    string       `json:"source"`
	Status    ThreadStatus `json:"status"`
	CreatedAt int64        `json:"createdAt"`
	UpdatedAt int64        `json:"updatedAt"`
}

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}

func (s ThreadStatus) Active() bool {
	return s.Type == "active"
}

type Turn struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	DurationMS *int64 `json:"durationMs"`
	Error      any    `json:"error"`
}

type Event struct {
	Method string
	Params json.RawMessage
}

// ServerRequest is a request initiated by Codex. Its ID must be returned to
// the same app-server connection.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

func (r ServerRequest) IDString() string {
	return string(r.ID)
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return fmt.Sprintf("app-server RPC error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("app-server RPC error %d: %s: %s", e.Code, e.Message, string(e.Data))
}

type ThreadStartInput struct {
	CWD   string
	Model string
}

type ThreadResumeInput struct {
	ThreadID string
	CWD      string
	Model    string
}

type TurnStartInput struct {
	ThreadID string
	Text     string
	CWD      string
	Model    string
}
