package transport

import (
	"context"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
)

type Receiver interface {
	Receive(ctx context.Context, handle func(context.Context, contracts.InboundEvent) error) error
}

type Sender interface {
	Send(ctx context.Context, msg contracts.OutboundMessage) (contracts.SentMessage, error)
}
