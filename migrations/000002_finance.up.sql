CREATE TABLE wagering.wallets (
 id uuid PRIMARY KEY,
 player_id uuid NOT NULL,
 currency text NOT NULL CHECK (currency = 'BRL'),
 balance bigint NOT NULL CHECK (balance >= 0),
 version bigint NOT NULL CHECK (version >= 1),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
 UNIQUE (player_id,currency), UNIQUE (id,currency), UNIQUE (id,player_id,currency)
);
CREATE TABLE wagering.transactions (
 id uuid PRIMARY KEY,
 origin text NOT NULL CHECK (origin IN ('INTERNAL','EXTERNAL')),
 provider_id text,
 external_id text,
 idempotency_key text,
 payload_hash text,
 wallet_id uuid NOT NULL,
 player_id uuid NOT NULL,
 currency text NOT NULL,
 round_id text,
 game_id text,
 kind text NOT NULL CHECK (kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
 amount bigint NOT NULL CHECK ((kind = 'LOSS' AND amount = 0) OR (kind <> 'LOSS' AND amount > 0)),
 reference_external_id text,
 reference_id uuid REFERENCES wagering.transactions(id),
 status text NOT NULL CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
 failure_code text,
 result_balance bigint CHECK (result_balance >= 0),
 direction text CHECK (direction IN ('DEBIT','CREDIT')),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 next_attempt_at timestamptz,
 expires_at timestamptz,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
 FOREIGN KEY (wallet_id,player_id,currency) REFERENCES wagering.wallets(id,player_id,currency),
 UNIQUE (provider_id,external_id), UNIQUE (provider_id,idempotency_key),
 UNIQUE (id,wallet_id,currency,amount),
 CHECK ((origin = 'INTERNAL' AND kind = 'OPENING' AND status = 'PROCESSED' AND provider_id IS NULL AND external_id IS NULL AND idempotency_key IS NULL AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL AND reference_external_id IS NULL AND reference_id IS NULL AND direction = 'CREDIT') OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING' AND length(provider_id) > 0 AND length(external_id) > 0 AND length(idempotency_key) > 0 AND length(payload_hash) = 64 AND length(round_id) > 0 AND length(game_id) > 0)),
 CHECK ((status IN ('REJECTED','FAILED') AND failure_code IS NOT NULL) OR (status NOT IN ('REJECTED','FAILED') AND failure_code IS NULL)),
 CHECK (status NOT IN ('PROCESSED','REJECTED','FAILED') OR result_balance IS NOT NULL),
 CHECK (status <> 'PROCESSED' OR ((kind = 'LOSS' AND direction IS NULL) OR (kind <> 'LOSS' AND direction IS NOT NULL))),
 CHECK (kind NOT IN ('REFUND','ROLLBACK') OR length(reference_external_id) > 0),
 CHECK (kind NOT IN ('REFUND','ROLLBACK') OR reference_external_id IS NOT NULL),
 CHECK (origin <> 'EXTERNAL' OR (provider_id IS NOT NULL AND external_id IS NOT NULL AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL AND round_id IS NOT NULL AND game_id IS NOT NULL)),
 CHECK (kind NOT IN ('BET','LOSS') OR reference_external_id IS NULL),
 CHECK (status <> 'PENDING_REFERENCE' OR (reference_external_id IS NOT NULL AND next_attempt_at IS NOT NULL AND expires_at IS NOT NULL))
);
CREATE UNIQUE INDEX one_opening ON wagering.transactions(wallet_id) WHERE kind = 'OPENING';
-- A successful direct REFUND or ROLLBACK consumes the referenced effect.
CREATE UNIQUE INDEX one_reversal ON wagering.transactions(reference_id) WHERE status = 'PROCESSED' AND kind IN ('REFUND','ROLLBACK');
CREATE INDEX pending_references ON wagering.transactions(next_attempt_at,id) WHERE status = 'PENDING_REFERENCE';
CREATE TABLE wagering.ledger (
 id uuid PRIMARY KEY,
 wallet_id uuid NOT NULL,
 transaction_id uuid NOT NULL,
 direction text NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
 amount bigint NOT NULL CHECK (amount > 0),
 currency text NOT NULL,
 balance_before bigint NOT NULL CHECK (balance_before >= 0),
 balance_after bigint NOT NULL CHECK (balance_after >= 0),
 wallet_version bigint NOT NULL CHECK (wallet_version >= 1),
 created_at timestamptz NOT NULL,
 FOREIGN KEY (transaction_id,wallet_id,currency,amount) REFERENCES wagering.transactions(id,wallet_id,currency,amount),
 UNIQUE (wallet_id,transaction_id),
 UNIQUE (wallet_id,wallet_version),
 CHECK (balance_after::numeric = balance_before::numeric + CASE direction WHEN 'CREDIT' THEN amount::numeric ELSE -amount::numeric END)
);
CREATE INDEX ledger_order ON wagering.ledger(wallet_id,created_at,id);
CREATE TABLE wagering.inbox (
 consumer_name text NOT NULL,
 message_id text NOT NULL,
 payload_hash text NOT NULL,
 received_at timestamptz NOT NULL,
 completed_at timestamptz,
 PRIMARY KEY (consumer_name,message_id)
);
CREATE TABLE wagering.outbox (
 event_id uuid PRIMARY KEY,
 aggregate_id uuid NOT NULL,
 event_type text NOT NULL,
 payload jsonb NOT NULL,
 occurred_at timestamptz NOT NULL,
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 next_attempt_at timestamptz NOT NULL,
 published_at timestamptz,
 lease_token uuid,
 lease_until timestamptz
);
CREATE INDEX pending_outbox ON wagering.outbox(next_attempt_at,event_id) WHERE published_at IS NULL;

CREATE FUNCTION wagering.protect_wallet() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'wallets cannot be deleted' USING ERRCODE = '23514'; END IF;
 IF (NEW.id,NEW.player_id,NEW.currency,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.player_id,OLD.currency,OLD.created_at) OR NEW.updated_at < OLD.updated_at THEN
  RAISE EXCEPTION 'wallet identity is immutable' USING ERRCODE = '23514';
 END IF;
 IF (NEW.balance <> OLD.balance AND NEW.version::numeric <> OLD.version::numeric + 1) OR
    (NEW.balance = OLD.balance AND NEW.version <> OLD.version) THEN
  RAISE EXCEPTION 'wallet version must follow balance changes' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER wallet_invariants BEFORE UPDATE OR DELETE ON wagering.wallets FOR EACH ROW EXECUTE FUNCTION wagering.protect_wallet();

CREATE FUNCTION wagering.protect_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'ledger is append-only' USING ERRCODE = '23514'; END $$;
CREATE TRIGGER immutable_ledger BEFORE UPDATE OR DELETE ON wagering.ledger FOR EACH ROW EXECUTE FUNCTION wagering.protect_ledger();
CREATE TRIGGER no_truncate_ledger BEFORE TRUNCATE ON wagering.ledger FOR EACH STATEMENT EXECUTE FUNCTION wagering.protect_ledger();

CREATE FUNCTION wagering.check_ledger_append() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_wallet wagering.wallets; previous_balance bigint; previous_version bigint;
BEGIN
 SELECT * INTO current_wallet FROM wagering.wallets WHERE id = NEW.wallet_id FOR UPDATE;
 SELECT balance_after,wallet_version INTO previous_balance,previous_version FROM wagering.ledger WHERE wallet_id = NEW.wallet_id ORDER BY wallet_version DESC LIMIT 1;
 IF NEW.balance_before <> COALESCE(previous_balance,0) OR NEW.balance_after <> current_wallet.balance OR NEW.wallet_version <> current_wallet.version OR
    NEW.wallet_version <> (CASE WHEN previous_version IS NOT NULL THEN previous_version + 1 WHEN NEW.wallet_version = 1 THEN 1 ELSE 2 END) THEN
  RAISE EXCEPTION 'ledger chain mismatch' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ledger_chain BEFORE INSERT ON wagering.ledger FOR EACH ROW EXECUTE FUNCTION wagering.check_ledger_append();

CREATE FUNCTION wagering.protect_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'transactions cannot be deleted' USING ERRCODE = '23514'; END IF;
 IF OLD.status IN ('PROCESSED','REJECTED','FAILED') THEN RAISE EXCEPTION 'terminal transaction is immutable' USING ERRCODE = '23514'; END IF;
 IF (to_jsonb(NEW) - ARRAY['status','failure_code','result_balance','direction','reference_id','attempts','next_attempt_at','updated_at']) IS DISTINCT FROM
    (to_jsonb(OLD) - ARRAY['status','failure_code','result_balance','direction','reference_id','attempts','next_attempt_at','updated_at']) THEN
  RAISE EXCEPTION 'transaction identity is immutable' USING ERRCODE = '23514';
 END IF;
 IF NEW.status = 'PENDING' THEN RAISE EXCEPTION 'invalid state transition' USING ERRCODE = '23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER immutable_transaction BEFORE UPDATE OR DELETE ON wagering.transactions FOR EACH ROW EXECUTE FUNCTION wagering.protect_transaction();

CREATE FUNCTION wagering.protect_outbox_payload() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.event_id,NEW.aggregate_id,NEW.event_type,NEW.payload,NEW.occurred_at) IS DISTINCT FROM
    (OLD.event_id,OLD.aggregate_id,OLD.event_type,OLD.payload,OLD.occurred_at) THEN
  RAISE EXCEPTION 'event snapshot is immutable' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER immutable_event BEFORE UPDATE ON wagering.outbox FOR EACH ROW EXECUTE FUNCTION wagering.protect_outbox_payload();

-- Deferred checks see the complete operation, regardless of SQL write order.
CREATE FUNCTION wagering.verify_wallet_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE wid uuid; stored bigint; calculated numeric;
BEGIN
 IF TG_TABLE_NAME = 'wallets' THEN wid := NEW.id; ELSE wid := NEW.wallet_id; END IF;
 SELECT balance INTO stored FROM wagering.wallets WHERE id = wid FOR UPDATE;
 SELECT COALESCE(sum(CASE direction WHEN 'CREDIT' THEN amount::numeric ELSE -amount::numeric END),0)
 INTO calculated FROM wagering.ledger WHERE wallet_id = wid;
 IF stored::numeric <> calculated THEN RAISE EXCEPTION 'wallet/ledger mismatch' USING ERRCODE = '23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER wallet_ledger_consistency AFTER INSERT OR UPDATE ON wagering.wallets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION wagering.verify_wallet_ledger();
CREATE CONSTRAINT TRIGGER ledger_wallet_consistency AFTER INSERT ON wagering.ledger DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION wagering.verify_wallet_ledger();

CREATE FUNCTION wagering.verify_transaction_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE op wagering.transactions; entry wagering.ledger; ref wagering.transactions;
BEGIN
 SELECT * INTO op FROM wagering.transactions WHERE id = NEW.id;
 SELECT * INTO entry FROM wagering.ledger WHERE transaction_id = op.id;
 IF op.status = 'PENDING' THEN RAISE EXCEPTION 'unrecoverable pending operation' USING ERRCODE = '23514'; END IF;
 IF op.status = 'PROCESSED' AND op.kind <> 'LOSS' THEN
  IF entry.id IS NULL OR entry.direction <> op.direction OR entry.balance_after <> op.result_balance THEN
   RAISE EXCEPTION 'processed operation requires matching ledger' USING ERRCODE = '23514';
  END IF;
  IF op.kind = 'OPENING' AND (entry.wallet_version <> 1 OR entry.balance_before <> 0) THEN
   RAISE EXCEPTION 'opening must be the initial credit' USING ERRCODE = '23514';
  END IF;
 ELSIF entry.id IS NOT NULL THEN RAISE EXCEPTION 'nonfinancial operation has ledger' USING ERRCODE = '23514'; END IF;
 IF op.status = 'PROCESSED' AND op.reference_external_id IS NOT NULL THEN
  SELECT * INTO ref FROM wagering.transactions WHERE id = op.reference_id;
  IF ref.id IS NULL OR ref.status <> 'PROCESSED' OR
     (op.provider_id,op.player_id,op.wallet_id,op.currency,op.round_id,op.game_id,op.reference_external_id) IS DISTINCT FROM
     (ref.provider_id,ref.player_id,ref.wallet_id,ref.currency,ref.round_id,ref.game_id,ref.external_id) THEN
   RAISE EXCEPTION 'invalid reference' USING ERRCODE = '23514';
  END IF;
  IF (op.kind = 'WIN' AND ref.kind <> 'BET') OR
     (op.kind = 'REFUND' AND (ref.kind <> 'BET' OR op.amount <> ref.amount OR op.direction <> 'CREDIT')) OR
     (op.kind = 'ROLLBACK' AND (ref.kind NOT IN ('BET','WIN','REFUND') OR op.amount <> ref.amount OR op.direction = ref.direction)) THEN
   RAISE EXCEPTION 'invalid reversal' USING ERRCODE = '23514';
  END IF;
 END IF;
 IF op.status = 'PROCESSED' AND ((op.kind = 'BET' AND op.direction <> 'DEBIT') OR (op.kind IN ('OPENING','WIN') AND op.direction <> 'CREDIT')) THEN
  RAISE EXCEPTION 'invalid direction' USING ERRCODE = '23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER transaction_ledger_consistency AFTER INSERT OR UPDATE ON wagering.transactions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION wagering.verify_transaction_ledger();
