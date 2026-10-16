DROP TABLE wagering.outbox, wagering.inbox, wagering.ledger;
DROP TABLE wagering.transactions;
DROP TABLE wagering.wallets;
DROP FUNCTION IF EXISTS wagering.protect_wallet(), wagering.protect_ledger(), wagering.check_ledger_append(), wagering.protect_transaction(), wagering.protect_outbox_payload(), wagering.verify_wallet_ledger(), wagering.verify_transaction_ledger();
