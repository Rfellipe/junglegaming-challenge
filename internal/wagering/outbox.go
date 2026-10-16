package wagering

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type Publication struct {
	EventID, Token, WalletID string
	Payload                  json.RawMessage
	Attempts                 int
}

func (s *Service) ClaimEvent(ctx context.Context) (Publication, error) {
	var p Publication
	p.Token = ID()
	err := s.pool().QueryRow(ctx, `UPDATE wagering.outbox SET lease_token=$1,lease_until=now()+interval '15 seconds',attempts=attempts+1 WHERE event_id=(SELECT event_id FROM wagering.outbox WHERE published_at IS NULL AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<=now()) ORDER BY next_attempt_at,event_id FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING event_id::text,payload,COALESCE(payload->'data'->>'walletId',aggregate_id::text),attempts`, p.Token).Scan(&p.EventID, &p.Payload, &p.WalletID, &p.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}
func (s *Service) CompleteEvent(ctx context.Context, p Publication, published bool) error {
	_, err := s.pool().Exec(ctx, `UPDATE wagering.outbox SET published_at=CASE WHEN $3 THEN now() ELSE NULL END,lease_token=NULL,lease_until=NULL,next_attempt_at=now()+make_interval(secs => LEAST(300,power(2,LEAST(attempts,8)))::int) WHERE event_id=$1 AND lease_token=$2 AND published_at IS NULL`, p.EventID, p.Token, published)
	return err
}
