-- Schema for the agent's Postgres memory store.
--
-- training_pairs holds learned mappings from raw SMS topics (merchant/counterparty names) to the
-- enriched expense fields the agent should use for future transactions with a similar topic.
--
-- bank_account_mappings holds static mappings from a bank account/card identifier (as printed in
-- the SMS) to the Cashew wallet it corresponds to.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE IF NOT EXISTS training_pairs (
    id              BIGSERIAL PRIMARY KEY,
    raw_topic       TEXT NOT NULL,
    name            TEXT NOT NULL,
    category_fk     TEXT NOT NULL,
    sub_category_fk TEXT,
    wallet_fk       TEXT NOT NULL,
    income          BOOLEAN NOT NULL DEFAULT FALSE,
    hit_count       INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (raw_topic, category_fk, wallet_fk)
);

CREATE INDEX IF NOT EXISTS training_pairs_raw_topic_trgm_idx
    ON training_pairs USING gin (raw_topic gin_trgm_ops);

CREATE TABLE IF NOT EXISTS bank_account_mappings (
    account_pattern TEXT PRIMARY KEY,
    wallet_fk       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
