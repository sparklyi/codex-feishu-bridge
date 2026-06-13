package feishu

import (
	"context"
	"errors"
	"time"

	"github.com/sihuo/codex-feishu-bridge/internal/contracts"
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
