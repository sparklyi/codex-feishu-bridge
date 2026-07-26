package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

// ErrRateLimited marks a delivery response that may succeed after waiting.
// It lives in the transport package so callers can apply the same retry
// policy without depending on a concrete delivery provider.
var ErrRateLimited = errors.New("delivery rate limited")

type Receiver interface {
	Receive(ctx context.Context, handle func(context.Context, contracts.InboundEvent) error) error
}

type Sender interface {
	Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error)
}

// IsTransientError reports whether a delivery failure can reasonably succeed on
// a later attempt without changing the request.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRateLimited) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) && temporary.Temporary() {
		return true
	}

	// The Feishu SDK formats some HTTP failures with %v rather than %w, so
	// errors.Is cannot reach the original timeout. Keep this fallback narrow.
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context deadline exceeded") ||
		strings.Contains(message, "client.timeout exceeded") ||
		strings.Contains(message, "i/o timeout") ||
		strings.Contains(message, "operation timed out") ||
		strings.Contains(message, "tls handshake timeout")
}
