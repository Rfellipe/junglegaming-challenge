// Package wagering coordinates exact financial operations in one PostgreSQL transaction.
package wagering

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"junglegaming/challenge/internal/domain"
)

var (
	ErrConflict = errors.New("idempotency or wallet conflict")
	ErrNotFound = errors.New("wallet not found")
)

type Service struct{ pool func() *pgxpool.Pool }

// A pool accessor permits Fx construction before the database lifecycle starts.
func New(pool func() *pgxpool.Pool) *Service { return &Service{pool: pool} }

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func ValidID(s string) bool { return uuidPattern.MatchString(s) }
func validText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsRune(s, 0)
}

type WalletView struct {
	ID       string       `json:"id"`
	PlayerID string       `json:"playerId"`
	Balance  domain.Money `json:"balance"`
	Version  int64        `json:"version"`
}

func view(w *domain.Wallet) WalletView {
	return WalletView{w.ID(), w.PlayerID(), w.Balance(), w.Version()}
}
func (s *Service) CreateWallet(ctx context.Context, player string, initial domain.Money) (WalletView, error) {
	if !ValidID(player) || !initial.Valid() || initial.Currency() != "BRL" || initial.Minor() < 0 {
		return WalletView{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	wallet, err := domain.NewWallet(ID(), strings.ToLower(player), initial, now)
	if err != nil {
		return WalletView{}, err
	}
	tx, err := s.pool().Begin(ctx)
	if err != nil {
		return WalletView{}, err
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `INSERT INTO wagering.wallets(id,player_id,currency,balance,version,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,$5)`, wallet.ID(), wallet.PlayerID(), initial.Currency(), initial.Minor(), now)
	if err != nil {
		return WalletView{}, classify(err)
	}
	if initial.Minor() > 0 {
		op := operation{id: ID(), request: Request{PlayerID: wallet.PlayerID(), WalletID: wallet.ID(), Kind: domain.Opening, Money: initial}, status: domain.Processed, created: now, updated: now}
		if _, err = domain.NewOpeningTransaction(op.id, initial, now); err != nil {
			return WalletView{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO wagering.transactions(id,origin,wallet_id,player_id,currency,kind,amount,status,result_balance,direction,created_at,updated_at) VALUES($1,'INTERNAL',$2,$3,$4,'OPENING',$5,'PROCESSED',$5,'CREDIT',$6,$6)`, op.id, wallet.ID(), wallet.PlayerID(), initial.Currency(), initial.Minor(), now)
		if err != nil {
			return WalletView{}, err
		}
		zero, _ := domain.Zero(initial.Currency())
		if err = ledger(ctx, tx, op, wallet, domain.Credit, zero, now); err != nil {
			return WalletView{}, err
		}
		if err = processedEvents(ctx, tx, op, wallet, domain.Credit, zero, now); err != nil {
			return WalletView{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return WalletView{}, err
	}
	return view(wallet), nil
}

func classify(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrConflict
	}
	return err
}

type scanner interface{ Scan(...any) error }

func scanWallet(row scanner) (*domain.Wallet, error) {
	var id, player, currency string
	var balance, version int64
	var created, updated time.Time
	err := row.Scan(&id, &player, &currency, &balance, &version, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m, err := domain.FromMinor(balance, currency)
	if err != nil {
		return nil, err
	}
	return domain.RestoreWallet(id, player, m, version, created, updated)
}

const walletColumns = `id::text,player_id::text,currency,balance,version,created_at,updated_at`

func (s *Service) Wallet(ctx context.Context, id string) (WalletView, error) {
	if !ValidID(id) {
		return WalletView{}, domain.ErrInvalid
	}
	w, err := scanWallet(s.pool().QueryRow(ctx, `SELECT `+walletColumns+` FROM wagering.wallets WHERE id=$1`, id))
	if err != nil {
		return WalletView{}, err
	}
	return view(w), nil
}

type Request struct {
	ProviderID          string       `json:"providerId"`
	ExternalID          string       `json:"externalTransactionId"`
	PlayerID            string       `json:"playerId"`
	WalletID            string       `json:"walletId"`
	RoundID             string       `json:"roundId"`
	GameID              string       `json:"gameId"`
	Kind                domain.Kind  `json:"kind"`
	Money               domain.Money `json:"money"`
	ReferenceExternalID string       `json:"referenceExternalTransactionId,omitempty"`
}

func (r Request) Validate(key string) error {
	if !validText(r.ProviderID) || !validText(r.ExternalID) || !validText(key) || !validText(r.RoundID) || !validText(r.GameID) || !ValidID(r.PlayerID) || !ValidID(r.WalletID) || r.Money.Currency() != "BRL" || (r.ReferenceExternalID != "" && !validText(r.ReferenceExternalID)) {
		return domain.ErrInvalid
	}
	return domain.ValidateExternal(r.Kind, r.Money, r.ReferenceExternalID)
}

// CanonicalHash uses lexically ordered JSON map keys and normalized UUIDs/Money.
// Transport metadata and the submitted idempotency key are deliberately excluded.
func CanonicalHash(r Request) (string, error) {
	if !r.Money.Valid() {
		return "", domain.ErrInvalid
	}
	money := map[string]string{"amount": r.Money.Amount(), "currency": r.Money.Currency()}
	b, err := json.Marshal(map[string]any{"providerId": r.ProviderID, "externalTransactionId": r.ExternalID, "playerId": strings.ToLower(r.PlayerID), "walletId": strings.ToLower(r.WalletID), "roundId": r.RoundID, "gameId": r.GameID, "kind": r.Kind, "money": money, "referenceExternalTransactionId": r.ReferenceExternalID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type Result struct {
	TransactionID    string        `json:"transactionId"`
	Status           domain.Status `json:"status"`
	Balance          domain.Money  `json:"balance"`
	FailureCode      string        `json:"failureCode,omitempty"`
	IdempotentReplay bool          `json:"idempotentReplay"`
}
type operation struct {
	id, key, hash    string
	request          Request
	status           domain.Status
	failure          string
	balance          *int64
	referenceID      string
	direction        domain.Direction
	attempts         int
	created, updated time.Time
	expires          *time.Time
}

const operationColumns = `id::text,COALESCE(provider_id,''),COALESCE(external_id,''),COALESCE(idempotency_key,''),COALESCE(payload_hash,''),wallet_id::text,player_id::text,currency,COALESCE(round_id,''),COALESCE(game_id,''),kind,amount,COALESCE(reference_external_id,''),COALESCE(reference_id::text,''),status,COALESCE(failure_code,''),result_balance,COALESCE(direction,''),attempts,created_at,updated_at,expires_at`

func scanOperation(row scanner) (operation, error) {
	var o operation
	var currency string
	var amount int64
	r := &o.request
	err := row.Scan(&o.id, &r.ProviderID, &r.ExternalID, &o.key, &o.hash, &r.WalletID, &r.PlayerID, &currency, &r.RoundID, &r.GameID, &r.Kind, &amount, &r.ReferenceExternalID, &o.referenceID, &o.status, &o.failure, &o.balance, &o.direction, &o.attempts, &o.created, &o.updated, &o.expires)
	if err != nil {
		return o, err
	}
	r.Money, err = domain.FromMinor(amount, currency)
	return o, err
}
func (o operation) result(replay bool, fallback domain.Money) Result {
	balance := fallback
	if o.balance != nil {
		balance, _ = domain.FromMinor(*o.balance, o.request.Money.Currency())
	}
	return Result{o.id, o.status, balance, o.failure, replay}
}

func findReplay(ctx context.Context, tx pgx.Tx, r Request, key, hash string) (*Result, error) {
	rows, err := tx.Query(ctx, `SELECT `+operationColumns+` FROM wagering.transactions WHERE provider_id=$1 AND (idempotency_key=$2 OR external_id=$3)`, r.ProviderID, key, r.ExternalID)
	if err != nil {
		return nil, err
	}
	var existing operation
	count := 0
	for rows.Next() {
		existing, err = scanOperation(rows)
		if err != nil {
			break
		}
		count++
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	if count != 1 || existing.hash != hash || existing.key != key || existing.request.ExternalID != r.ExternalID {
		return nil, ErrConflict
	}
	fallback, _ := domain.Zero(r.Money.Currency())
	if existing.balance == nil {
		w, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wagering.wallets WHERE id=$1`, existing.request.WalletID))
		if err != nil {
			return nil, err
		}
		fallback = w.Balance()
	}
	result := existing.result(true, fallback)
	return &result, nil
}

func (s *Service) Process(ctx context.Context, r Request, key string) (Result, error) {
	return s.process(ctx, r, key, "", "")
}

// ProcessMessage commits inbox completion in the same transaction as processing.
func (s *Service) ProcessMessage(ctx context.Context, r Request, key, messageID, hash string) (Result, error) {
	if !validText(messageID) || len(hash) != 64 {
		return Result{}, domain.ErrInvalid
	}
	return s.process(ctx, r, key, messageID, hash)
}
func (s *Service) process(ctx context.Context, r Request, key, messageID, messageHash string) (Result, error) {
	if err := r.Validate(key); err != nil {
		return Result{}, err
	}
	r.PlayerID, r.WalletID = strings.ToLower(r.PlayerID), strings.ToLower(r.WalletID)
	hash, err := CanonicalHash(r)
	if err != nil {
		return Result{}, err
	}
	tx, err := s.pool().Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(context.Background())
	if messageID != "" {
		_, err = tx.Exec(ctx, `INSERT INTO wagering.inbox(consumer_name,message_id,payload_hash,received_at) VALUES('wagering-v1',$1,$2,now()) ON CONFLICT (consumer_name,message_id) DO UPDATE SET deliveries=wagering.inbox.deliveries+1`, messageID, messageHash)
		if err != nil {
			return Result{}, err
		}
		var persisted string
		if err = tx.QueryRow(ctx, `SELECT payload_hash FROM wagering.inbox WHERE consumer_name='wagering-v1' AND message_id=$1 FOR UPDATE`, messageID).Scan(&persisted); err != nil {
			return Result{}, err
		}
		if persisted != messageHash {
			return Result{}, ErrConflict
		}
	}
	finish := func(result Result) (Result, error) {
		if messageID != "" {
			if _, err := tx.Exec(ctx, `UPDATE wagering.inbox SET completed_at=COALESCE(completed_at,now()) WHERE consumer_name='wagering-v1' AND message_id=$1`, messageID); err != nil {
				return Result{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, classify(err)
		}
		return result, nil
	}
	// Check persisted identity before validating a changed wallet/player. A reused
	// key must conflict even if the replacement wallet does not exist.
	if replay, err := findReplay(ctx, tx, r, key, hash); err != nil {
		return Result{}, err
	} else if replay != nil {
		return finish(*replay)
	}
	// This is the sole financial coordination lock, shared across all processes.
	w, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wagering.wallets WHERE id=$1 FOR UPDATE`, r.WalletID))
	if err != nil {
		return Result{}, err
	}
	if w.PlayerID() != r.PlayerID || w.Balance().Currency() != r.Money.Currency() {
		return Result{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	id := ID()
	tag, err := tx.Exec(ctx, `INSERT INTO wagering.transactions(id,origin,provider_id,external_id,idempotency_key,payload_hash,wallet_id,player_id,currency,round_id,game_id,kind,amount,reference_external_id,status,created_at,updated_at,expires_at) VALUES($1,'EXTERNAL',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),'PENDING',$14,$14,$15) ON CONFLICT DO NOTHING`, id, r.ProviderID, r.ExternalID, key, hash, r.WalletID, r.PlayerID, r.Money.Currency(), r.RoundID, r.GameID, r.Kind, r.Money.Minor(), r.ReferenceExternalID, now, now.Add(24*time.Hour))
	if err != nil {
		return Result{}, err
	}
	if tag.RowsAffected() == 0 {
		replay, err := findReplay(ctx, tx, r, key, hash)
		if err != nil {
			return Result{}, err
		}
		if replay == nil {
			return Result{}, ErrConflict
		}
		return finish(*replay)
	}
	o := operation{id: id, key: key, hash: hash, request: r, status: domain.Pending, created: now, updated: now}
	expiry := now.Add(24 * time.Hour)
	o.expires = &expiry
	if err = s.apply(ctx, tx, &o, w, now, false); err != nil {
		return Result{}, err
	}
	return finish(o.result(false, w.Balance()))
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, o *operation, w *domain.Wallet, now time.Time, retry bool) error {
	state, err := domain.RestoreTransaction(o.id, o.request.Kind, o.status, o.request.Money, o.failure, o.request.ReferenceExternalID, o.updated)
	if err != nil {
		return err
	}
	r := o.request
	failure := ""
	waiting := false
	if retry && o.expires != nil && !now.Before(*o.expires) {
		failure = domain.ReferenceNotFound
	}
	direction := domain.Credit
	if r.Kind == domain.Bet {
		direction = domain.Debit
	}
	if r.Kind == domain.Loss {
		direction = ""
	}
	if r.ReferenceExternalID != "" && failure == "" {
		ref, err := scanOperation(tx.QueryRow(ctx, `SELECT `+operationColumns+` FROM wagering.transactions WHERE provider_id=$1 AND external_id=$2`, r.ProviderID, r.ReferenceExternalID))
		if errors.Is(err, pgx.ErrNoRows) {
			waiting = true
		} else if err != nil {
			return err
		} else {
			resolution := domain.ResolveReference(eventData(*o), domain.Reference{TransactionData: eventData(ref), Status: ref.status, Direction: ref.direction})
			failure, waiting = resolution.FailureCode, resolution.Waiting
			if failure == "" && !waiting {
				o.referenceID = ref.id
				direction = resolution.Direction
				if r.Kind == domain.Refund || r.Kind == domain.Rollback {
					var reversed bool
					if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wagering.transactions WHERE reference_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK'))`, ref.id).Scan(&reversed); err != nil {
						return err
					}
					if reversed {
						failure = domain.AlreadyReversed
					}
				}
			}
		}
	}
	if waiting {
		if retry {
			o.attempts++
		}
		if o.attempts >= 12 || (o.expires != nil && !now.Before(*o.expires)) {
			failure = domain.ReferenceNotFound
		} else {
			if err = state.Transition(domain.PendingReference, "", now); err != nil {
				return err
			}
			first := o.status != domain.PendingReference
			o.status = domain.PendingReference
			shift := min(o.attempts, 8)
			delay := time.Second * time.Duration(1<<shift)
			_, err = tx.Exec(ctx, `UPDATE wagering.transactions SET status='PENDING_REFERENCE',attempts=$2,next_attempt_at=$3,updated_at=$4 WHERE id=$1`, o.id, o.attempts, now.Add(delay), now)
			if err != nil {
				return err
			}
			if first {
				return putEvent(ctx, tx, domain.NewPendingReferenceEvent(ID(), o.id, now, domain.PendingReferenceData{TransactionData: eventData(*o), ReferenceExternalID: r.ReferenceExternalID}))
			}
			return nil
		}
	}
	before := w.Balance()
	if failure == "" && direction != "" {
		err = w.Move(direction, r.Money, now)
		switch {
		case errors.Is(err, domain.ErrInsufficientFunds):
			if r.Kind == domain.Bet {
				failure = domain.InsufficientFunds
			} else {
				failure = domain.ReversalInsufficientFunds
			}
		case errors.Is(err, domain.ErrOverflow):
			failure = domain.BalanceOverflow
		case err != nil:
			return err
		}
	}
	o.status = domain.Processed
	o.failure = failure
	if failure != "" {
		o.status = domain.Rejected
		direction = ""
	}
	if err = state.Transition(o.status, failure, now); err != nil {
		return err
	}
	amount := w.Balance().Minor()
	o.balance = &amount
	o.direction = direction
	_, err = tx.Exec(ctx, `UPDATE wagering.transactions SET status=$2,failure_code=NULLIF($3,''),result_balance=$4,direction=NULLIF($5,''),reference_id=NULLIF($6,'')::uuid,attempts=$7,next_attempt_at=NULL,updated_at=$8 WHERE id=$1`, o.id, o.status, failure, amount, string(direction), o.referenceID, o.attempts, now)
	if err != nil {
		return err
	}
	if failure != "" {
		return putEvent(ctx, tx, domain.NewRejectedEvent(ID(), o.id, now, domain.RejectedData{TransactionData: eventData(*o), FailureCode: failure}))
	}
	if direction != "" {
		_, err = tx.Exec(ctx, `UPDATE wagering.wallets SET balance=$2,version=$3,updated_at=$4 WHERE id=$1`, w.ID(), amount, w.Version(), now)
		if err != nil {
			return err
		}
		if err = ledger(ctx, tx, *o, w, direction, before, now); err != nil {
			return err
		}
	}
	return processedEvents(ctx, tx, *o, w, direction, before, now)
}

func ledger(ctx context.Context, tx pgx.Tx, o operation, w *domain.Wallet, direction domain.Direction, before domain.Money, now time.Time) error {
	id := ID()
	if _, err := domain.NewLedgerEntry(id, w.ID(), o.id, direction, o.request.Money, before, w.Balance(), now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO wagering.ledger(id,wallet_id,transaction_id,direction,amount,currency,balance_before,balance_after,wallet_version,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, w.ID(), o.id, direction, o.request.Money.Minor(), o.request.Money.Currency(), before.Minor(), w.Balance().Minor(), w.Version(), now)
	return err
}
func eventData(o operation) domain.TransactionData {
	r := o.request
	return domain.TransactionData{TransactionID: o.id, WalletID: r.WalletID, PlayerID: r.PlayerID, ProviderID: r.ProviderID, ExternalID: r.ExternalID, RoundID: r.RoundID, GameID: r.GameID, Kind: r.Kind, Money: r.Money}
}
func processedEvents(ctx context.Context, tx pgx.Tx, o operation, w *domain.Wallet, direction domain.Direction, before domain.Money, now time.Time) error {
	if err := putEvent(ctx, tx, domain.NewProcessedEvent(ID(), o.id, now, domain.ProcessedData{TransactionData: eventData(o), Balance: w.Balance()})); err != nil {
		return err
	}
	if direction == "" {
		return nil
	}
	return putEvent(ctx, tx, domain.NewBalanceChangedEvent(ID(), o.id, now, domain.BalanceChangedData{WalletID: w.ID(), TransactionID: o.id, Direction: direction, Money: o.request.Money, BalanceBefore: before, BalanceAfter: w.Balance(), WalletVersion: w.Version()}))
}
func putEvent[T any](ctx context.Context, tx pgx.Tx, event domain.Event[T]) error {
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO wagering.outbox(event_id,aggregate_id,event_type,payload,occurred_at,next_attempt_at) VALUES($1,$2,$3,$4,$5,$5)`, event.EventID, event.AggregateID, event.EventType, b, event.OccurredAt)
	return err
}

// RetryReference does no external I/O. Wallet then operation locks remain held
// until commit; a crash rolls back the entire attempt, immediately releasing work.
func (s *Service) RetryReference(ctx context.Context) (bool, error) {
	tx, err := s.pool().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	// Inspect a batch so a busy wallet does not monopolize the worker.
	rows, err := tx.Query(ctx, `SELECT id::text,wallet_id::text FROM (
		SELECT DISTINCT ON (wallet_id) id,wallet_id,next_attempt_at FROM wagering.transactions
		WHERE status='PENDING_REFERENCE' AND next_attempt_at <= now()
		ORDER BY wallet_id,next_attempt_at,id
	) candidates ORDER BY next_attempt_at,id LIMIT 32`)
	if err != nil {
		return false, err
	}
	type candidate struct{ id, wallet string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.wallet); err != nil {
			rows.Close()
			return false, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	for _, c := range candidates {
		w, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wagering.wallets WHERE id=$1 FOR UPDATE SKIP LOCKED`, c.wallet))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		o, err := scanOperation(tx.QueryRow(ctx, `SELECT `+operationColumns+` FROM wagering.transactions WHERE id=$1 AND status='PENDING_REFERENCE' AND next_attempt_at <= now() FOR UPDATE SKIP LOCKED`, c.id))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err = s.apply(ctx, tx, &o, w, time.Now().UTC(), true); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	return false, nil
}
