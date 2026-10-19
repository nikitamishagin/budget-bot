# End-to-end scenarios

Test data for end-to-end tests that play the role of a Telegram user (via
`internal/telegram/telegramtest`) against the real application composition (`internal/app`): LLM
parser, enrichment, Postgres memory and the Cashew SQLite store. Only the Telegram client is
faked. The runner is `e2e_integration_test.go` (build tag `integration`); scenarios are loaded by
`scenario.go`.

## Layout

One folder per scenario under `testdata/`:

```
testdata/<scenario>/
  database.sqlite      starting Cashew SQLite file (sent first by the user)
  screenshots/         *.jpg / *.jpeg / *.png, sent in file name order (use 01-, 02-, ... prefixes)
  memory.sql           Postgres memory script, applied before the scenario (required, may be empty)
  expected.json        expected results
```

Add a scenario by dropping these files into a new folder; `e2e.LoadAll("testdata")` picks it up.
`testdata/example` is a tiny synthetic scenario used only by the loader unit test.

`expected.json`:

```json
{
  "description": "free text",
  "draft": false,
  "transactions": [{"name": "Coffee", "amount": -3.5, "category": "Food", "wallet": "Cash", "note": "COFFEE SHOP"}],
  "reply_contains": ["Save these transactions"]
}
```

Category and wallet are names (not keys), so the file is readable.

## Drafting expected results from a real run

1. Run the agent on the scenario once with the fake user and write the resulting transactions
   (from the returned database) to `expected.json` with `"draft": true`.
2. Review every entry manually. A draft may contain the current bugs of the agent, so it must not
   be trusted as is. Set `"draft": false` only after review.

## Config

`config.test.yaml` holds the LLM, Postgres memory (`dbname: expense_agent_test`) and a stub
Telegram token. Override the path with `BUDGET_BOT_TEST_CONFIG`; the password comes from
`POSTGRES_PASSWORD`. The runner refuses to run if `memory.dbname` does not end with `_test`.

## memory.sql

Applied to the test database before each scenario; it must reset the memory tables and insert
that scenario's learned rows. Copy this template and adjust:

```sql
DROP TABLE IF EXISTS wallet_mappings;
DROP TABLE IF EXISTS transaction_mappings;
-- Recreate the tables: copy the DDL from internal/memory/schema.sql
-- (CREATE EXTENSION pg_trgm, wallet_mappings, transaction_mappings, trigram index).

-- Learned wallet: card pattern + currency -> Cashew wallet
-- INSERT INTO wallet_mappings (account_pattern, currency, name, type, wallet_fk)
-- VALUES ('*0746', 'USD', 'Cash', 'bank', '<wallet pk from database.sqlite>');

-- Learned transaction: merchant pattern -> name/category
-- INSERT INTO transaction_mappings (transaction_pattern, name, category_fk, sub_category_fk)
-- VALUES ('COFFEE SHOP', 'Coffee', '<category pk>', NULL);
```

An empty file (or comments only) means empty memory only if the tables already exist; prefer
including the DROP/CREATE part.

## Adding a scenario

1. Create `testdata/<name>/` with `database.sqlite`, `screenshots/`, `memory.sql`, `expected.json`.
2. Run the suite (below). The synthetic `example` scenario is always skipped by the runner.

## Running

```
go test ./tests/integrated/e2e/...                                  # loader tests, no DB
set -a && . ./postgres.env && set +a
go test -tags=integration -count=1 -v ./tests/integrated/e2e/...    # needs Postgres + LLM
```

Scenarios run sequentially because they share one database. Never use production data,
credentials or the real Telegram API.
