# Expense Agent

## Project goal

Expense Agent is a Go daemon that automates personal expense tracking. It runs on a local machine and ties together: an
LLM for parsing, a local SQLite database, Telegram bot (as a source of data about expenses).

## Core responsibilities

- Telegram bot — agent connected to Telegram bot API, receives an SQLite data file, screenshots from the user, sends
  questions or results back to the user.
- LLM parsing — sends raw screenshots to a local LLM (via llama.cpp OpenAI-compatible API) and gets back structured
  expense data (amount, currency, merchant, date, card number).
- SQLite editor — reads data from the database file, inserts the new data (expenses) into the database file.
- Finding a suitable topic — find a suitable topic for the expense in the database (e.g., expense with the same
  description in the database) to provide additional context for the LLM, if the agent can't find exact matches.
- Memorizing — keep track of matching kinds of expenses and categories, card numbers and bank accounts, specific cases,
  etc. to learn more and more how to handle expenses.
- Duplicate/conflict handling — before/after insert, detect duplicate entries (e.g., same expenses, same transaction
  twice) and resolve conflicts in the table.

## Tech stack

- Language: Go version 1.26.6
- LLM runtime: llama.cpp, OpenAI-compatible API, local inference
    - Model: Qwen3.5-9B-NVFP4-GGUF (swappable — don't hardcode the model name where avoidable)
    - Hardware: RTX 5070 12GB VRAM, 16GB RAM — keep this in mind for context window / concurrency limits when designing
      LLM calls
    - Context window: 128000 tokens
- Storage: SQLite (single file)
- External services: Telegram Bot API
- Agent memory: Postgres database
- OS: Ubuntu 26.04

Frameworks:

- CMD: cobra
- Tests: gomega amd ginkgo
- Logging: zerolog
- Config: yaml and viper

## Architecture / project layout

Each responsibility below is its own package under `internal/`, developed and tested independently before being wired
together end-to-end:

- Only one root command to run the agent as a daemon. Flag `--config` to provide the path to the config file
- bin/ - path to test binaries
- `cmd/agent` — Cobra entrypoint; the agent runs as a single daemon, `--config` selects the config file path.
- `internal/telegram` — Telegram bot backend: receives the SQLite file and screenshots, sends questions/results back to
  the user.
- `internal/llmparser` — LLM prompts construction and parsing logic: sends screenshots to the local LLM and returns
  structured `Expense` data; also resolves ambiguous topics/categories via LLM comparison.
- `internal/memory` — Postgres-backed agent memory: learned topic/category/wallet mappings, bank account mappings,
  similarity search over past decisions.
- `internal/handler` — Enrichment loop: finds suitable topics using memory/LLM/SQLite lookups, decides when a user
  question is required, aggregates results into the final approval message.
- `internal/store` — SQLite access: inserting parsed expenses, duplicate/conflict detection before and after insert.
- `internal/config` — YAML + Viper configuration loading (LLM, memory, telegram sections).
- something else if it can be helpful

## Go coding conventions and best practices

- Follow standard Go idioms: run `gofmt`/`goimports` before every commit; keep `golangci-lint` clean.
- Prefer simplicity over abstraction — avoid introducing interfaces or extra layers until at least two concrete
  implementations exist.
- Return errors, don't panic, in library/package code; wrap errors with `fmt.Errorf("...: %w", err)` to preserve
  context, and use `errors.Is`/`errors.As` (or `errors.AsType[T]`) for error inspection.
- Use `context.Context` as the first parameter for any function doing I/O (HTTP calls, DB queries) and respect
  cancellation/timeouts.
- Keep exported APIs minimal; unexported types and functions by default, export only what other packages actually need.
- Table-driven tests with Ginkgo/Gomega for anything with multiple branches or edge cases (refund vs. expense vs.
  transfer vs. failed parse, duplicate vs. new transaction, etc.).
- Structured logging via `zerolog` with appropriate levels (`Debug` for request/response bodies, `Info` for high-level
  lifecycle events, `Warn`/`Error` for recoverable/unrecoverable failures) — never log secrets (API keys, bot tokens) or
  full user financial data at `Info` level or above.
- Prefer modern Go 1.26 idioms where they simplify code: `slices`/`maps` packages, `min`/`max`, `for i := range n`,
  `errors.Join`, `t.Context()` in tests, `wg.Go` for goroutines spawned from a `sync.WaitGroup`, and `omitzero` (not
  `omitempty`) for `time.Time`/`time.Duration`/struct/slice/ map JSON fields.
- Keep configuration (secrets, settings) in the YAML config file loaded via viper — never hardcode tokens, API keys,
  hosts, or model names in code.
- One package, one responsibility: don't reach across `internal/` package boundaries except through their public API;
  keep `internal/handler` as the only place that orchestrates `internal/memory`, `internal/llmparser`, and
  `internal/store` together.

## LLM parsing notes

- Input: screenshots of SMS messages, in varying formats.
- Output raw JSON schema: `{amount, currency, bank_account, date, topic}` (plus `type`, `parse_failed`,
  `failure_reason` — see `internal/llmparser/types.go`).
- Edge cases handled: normal expense, refund, failed parse, transfer between accounts, income.

## Known constraints / things to remember

- The LLM must parse images to extract SMS data about expenses.
- An SQLite file must be received from the user before adding new expenses.
- After changing an SQLite file and getting a user approval, the agent must return the updated file to the user in a
  Telegram message.

## The main loop

1. The user sends an SQLite database file to the Telegram bot.
2. The agent receives the file and checks it. If it isn't an SQLite file, the agent returns an error message. If it is,
   the agent saves the file to the data directory.
3. The agent sends a message about waiting for screenshots. The user can send screenshots as a group (multiple in one
   message) or one image at a time.
4. When the user has uploaded all images, they enter `/parse`. This signals the agent to start parsing.
5. Received screenshots are saved to a queue and parsed one by one.
6. The agent aggregates the LLM's answers (raw SMS data) into one list.
7. The agent enriches the data to match the expected structure: it first searches memory for exact matches (expense
   name/category by description, wallet by card number). If found, it enriches the element directly. Every element is
   handled one by one.
8. If memory doesn't have an exact match, the agent searches the SQLite data file. If still no exact match, it gets
   several best (fuzzy/substring) matches and sends a request to the LLM.
9. The LLM compares the candidate options to pick a suitable title, category, and wallet. On success, the agent enriches
   the element and saves the new mapping to memory.
10. If the LLM isn't confident (> 90%) about any option, the agent prepares a question for the user about that specific
    expense (name and category, or wallet name for a bank number). One unknown expense is one request; questions are
    asked gradually.
11. The user answers with an expense name and category (e.g., "poppy coffee", "coffee"), or a wallet name.
12. The agent enriches the unknown expense using the answers, saves the new mapping to memory, and re-checks other
    unknown expenses against the newly learned data (so, e.g., several expenses from the same unknown place only need
    one question).
13. Once the full list is enriched, the agent sends it to the user and asks for approval (the message includes examples
    of how to answer). If approved, the agent writes the data to the SQLite file.
14. If not approved, the agent saves the list as unapproved data so the user can edit/fix it.
15. If the SQLite database is updated successfully, the agent sends the updated file back to the user. The cycle is
    complete.
16. To repeat the cycle, the user sends a new SQLite database file.

## Data schemas

Expected raw data from LLM:

- type - TransactionType
- amount - float64
- currency - string
- bank_account - string
- date - string
- description - string
- parse_failed - bool
- failure_reason - string, omitempty

Expected expenses data after enrichment with additional data from memory:

- transaction_pk - string
- paired_transaction_fk - string
- name - string
- amount - float64
- note - string
- category_fk - string
- sub_category_fk - string
- wallet_fk - string
- date_created - int64
- date_time_modified - int64
- original_date_due - int64
- income - bool
- period_length - int64
- reoccurrence - int64
- end_date - int64
- upcoming_transaction_notification - bool
- type - int64
- paid - bool
- created_another_future_transaction - bool
- skip_paid - bool
- method_added - int64
- transaction_owner_email - string
- transaction_original_owner_email - string
- shared_key - string
- shared_old_key - string
- shared_status - int64
- shared_date_updated - int64
- shared_reference_budget_pk - string
- objective_fk - string
- objective_loan_fk - string
- budget_fks_exclude - string

Only records matching this structure can be inserted into the database.

## Agent Memory (PostgreSQL)

The agent continuously learns from the user's financial transactions — wallets, categories, and subcategories — and
persists this knowledge as structured mappings in its memory. Before a transaction is inserted into the database, the
agent uses this memory to enrich it with additional derived fields, resolving details the raw source data doesn't
provide.

Memory is backed by a PostgreSQL instance. On startup, the agent checks that the memory database is reachable and
creates any required tables that don't yet exist. It must never wipe or overwrite existing data in the process.

### Table: wallet_mappings

```go
type WalletMappings struct {
    AccountPattern string `db:"account_pattern"` // pattern from raw SMS, e.g. *0746, **3421, 190-126240-52
    Name           string `db:"name"`            // must match "name" in sqlite "wallets" table
    Currency       string `db:"currency"`  // wallet's main currency; other currencies possible but unlikely
    Type           string `db:"type"`      // "bank" or "cash"
    WalletFK       string `db:"wallet_fk"` // FK to sqlite "wallets" table
}
```

### Table: transaction_mappings

```go
type TransactionMappings struct {
    TransactionPattern              string  `db:"transaction_pattern"` // merchant/counterparty/account name from raw SMS
    PairedTransactionFK             *string `db:"paired_transaction_fk"`
    Name                            *string `db:"name"`
    Note                            *string `db:"note"`
    CategoryFK                      *string `db:"category_fk"`
    SubCategoryFK                   *string `db:"sub_category_fk"`
    Income                          *int64  `db:"income"`
    PeriodLength                    *int64  `db:"period_length"`
    Reoccurrence                    *int64  `db:"reoccurrence"`
    EndDate                         *int64  `db:"end_date"`
    UpcomingTransactionNotification *int64  `db:"upcoming_transaction_notification"`
    Type                            *int64  `db:"type"`
    Paid                            *int64  `db:"paid"`
    CreatedAnotherFutureTransaction *int64  `db:"created_another_future_transaction"`
    SkipPaid                        *int64  `db:"skip_paid"`
    ObjectiveFK                     *string `db:"objective_fk"`
    ObjectiveLoanFK                 *string `db:"objective_loan_fk"`
}
```

### Field resolution rule (critical)

These `transaction_mappings` fields have no source in raw SMS data and are never provided by the user:

- PairedTransactionFK
- PeriodLength
- Reoccurrence
- EndDate
- UpcomingTransactionNotification
- Type
- Paid
- CreatedAnotherFutureTransaction
- SkipPaid
- ObjectiveFK
- ObjectiveLoanFK

Rule: For these fields, the agent must look up the same or a similar existing transaction in the sqlite
`transactions` table and copy their values from there. Do not leave them as agent-invented defaults; do not ask the
user. If no matching/similar transaction exists, leave as NULL.

## Committing rules

Follow this workflow for every change, step-by-step. Do not skip steps or batch them.

1. Implement one logical change at a time (single feature/fix per commit).
2. Lint: run `gofmt -l .` and `go vet ./...`. Fix any issues before proceeding.
3. Test: run `go test ./...`. All tests must pass. If tests fail, fix the code — unless the change was significant
   enough that the tests themselves are now outdated, in which case update the tests to match the new expected behavior.
4. Diff review: show `git diff --stat` and a summary of what changed and why.
5. Commit message: generate using the format below.
6. Ask for approval: wait for explicit confirmation before running `git commit`.
7. Never run `git push`. Pushing is always manual and done by me.

```
<type>(<scope>): <short summary>

<body: what changed and why, 1-2 lines>
```

Commit message prompt:

- Types: `feat`, `fix`, `refactor`, `test`, `chore`, `docs`.
- Scope = affected package/module (e.g. `telegram`, `parser`, `memory`, `cashew-sync`).
- Short summary must be no more than 50 characters.
- Body is 1-2 short sentences.
- Avoid overly verbose descriptions or unnecessary details.
- Don't use colon and semicolon in the description.


If lint or tests fail and cannot be fixed within the current change scope, stop and report the issue instead of
committing.
