# End-to-end scenarios

Test data for end-to-end tests that play the role of a Telegram user (via
`internal/telegram/telegramtest`) against the real application composition (`internal/app`): LLM
parser, enrichment, Postgres memory and the Cashew SQLite store. Only the Telegram client is
faked. The e2e test itself is added later, once real data is provided; this folder defines where
the data goes and how it is loaded (`scenario.go`).

## Layout

One folder per scenario under `testdata/`:

```
testdata/<scenario>/
  database.sqlite      starting Cashew SQLite file (sent first by the user)
  screenshots/         *.jpg / *.jpeg / *.png, sent in file name order (use 01-, 02-, ... prefixes)
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

## Rules for the future e2e test

- Build tag `integration`; run with `go test -tags=integration ./tests/integrated/e2e/...`.
- Use the dedicated Postgres database `expense_agent_test` (same helpers/env as
  `tests/integrated/enrichment`: `BUDGET_BOT_TEST_CONFIG`, `MEMORY_TEST_DBNAME`) and refuse to run
  if the database name is not the test one.
- Never use production data, real credentials or the real Telegram API.
