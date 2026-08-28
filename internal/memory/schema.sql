-- Schema for the agent's Postgres memory store.
--
-- wallet_mappings holds static mappings from a bank account/card identifier (as printed in the
-- SMS) and its currency to the Cashew wallet it corresponds to.
--
-- transaction_mappings holds learned mappings from a raw SMS transaction pattern (merchant/
-- counterparty name) to the enriched transaction fields the agent should reuse for future
-- transactions with a similar pattern.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Mappings are keyed by (account_pattern, currency), not just account_pattern: a single card can
-- produce expenses in several currencies (e.g. "TBC * (RSD)" vs "TBC * (USD)"), and each must
-- resolve to its own wallet.
CREATE TABLE IF NOT EXISTS wallet_mappings (
    account_pattern TEXT NOT NULL,
    currency        TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL,
    type            TEXT NOT NULL,
    wallet_fk       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_pattern, currency)
);

-- Most columns are nullable: several fields (paired_transaction_fk, period_length, reoccurrence,
-- end_date, upcoming_transaction_notification, type, paid, created_another_future_transaction,
-- skip_paid, objective_fk, objective_loan_fk) have no source in the raw SMS data and are never
-- filled in by the user, so they are only ever populated by copying them from a similar existing
-- sqlite transaction, and stay NULL when no such transaction exists.
CREATE TABLE IF NOT EXISTS transaction_mappings (
    id                                  BIGSERIAL PRIMARY KEY,
    transaction_pattern                 TEXT NOT NULL,
    paired_transaction_fk               TEXT,
    name                                TEXT,
    note                                TEXT,
    category_fk                         TEXT,
    sub_category_fk                     TEXT,
    income                              BIGINT,
    period_length                       BIGINT,
    reoccurrence                        BIGINT,
    end_date                            BIGINT,
    upcoming_transaction_notification   BIGINT,
    type                                BIGINT,
    paid                                BIGINT,
    created_another_future_transaction  BIGINT,
    skip_paid                           BIGINT,
    objective_fk                        TEXT,
    objective_loan_fk                   TEXT,
    hit_count                           INTEGER NOT NULL DEFAULT 1,
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transaction_pattern)
);

CREATE INDEX IF NOT EXISTS transaction_mappings_pattern_trgm_idx
    ON transaction_mappings USING gin (transaction_pattern gin_trgm_ops);

-- Earlier versions of this schema used differently-shaped tables (training_pairs,
-- bank_account_mappings). Drop them once the new tables are in place, rather than leaving dead
-- tables around; there is no compatible column mapping to migrate their rows into the new shape.
DROP TABLE IF EXISTS training_pairs;
DROP TABLE IF EXISTS bank_account_mappings;
