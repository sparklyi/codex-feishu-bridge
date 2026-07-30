package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const maxCardStreamSequence = 2147483647

// ReserveCardStreamSequence reserves a strictly increasing sequence range for
// one CardKit card. Reserving before use keeps the next activation valid after
// a bridge restart or a continuation on the same task card.
func (s *Store) ReserveCardStreamSequence(ctx context.Context, messageID string, minimum, count int) (int, error) {
	if strings.TrimSpace(messageID) == "" {
		return 0, errors.New("card stream message id is required")
	}
	if minimum < 1 || minimum > maxCardStreamSequence {
		return 0, fmt.Errorf("card stream minimum sequence %d is outside the supported range", minimum)
	}
	if count < 1 || count > maxCardStreamSequence {
		return 0, fmt.Errorf("card stream sequence reservation count %d is invalid", count)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)

	last := 0
	err = tx.QueryRowContext(ctx, `SELECT last_sequence FROM card_stream_sequences WHERE feishu_message_id=?`, messageID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	start := minimum
	if last >= start {
		start = last + 1
	}
	if start > maxCardStreamSequence || count-1 > maxCardStreamSequence-start {
		return 0, errors.New("card stream sequence range is exhausted")
	}
	last = start + count - 1
	if _, err := tx.ExecContext(ctx, `
INSERT INTO card_stream_sequences(feishu_message_id,last_sequence)
VALUES(?,?)
ON CONFLICT(feishu_message_id) DO UPDATE SET last_sequence=excluded.last_sequence`, messageID, last); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return start, nil
}
