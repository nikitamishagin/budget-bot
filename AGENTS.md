# Expense Agent

## Project goal

Expense Agent is a Go daemon that automates personal expense tracking.

## Core responsibilities

- Telegram bot — agent connected to Telegram bot API, receives an SQLite data file, screenshots from the user, sends
  questions or results back to the user.
- LLM parsing — sends raw screenshots to a local LLM (via llama.cpp OpenAI-compatible API) and gets back structured
  expense data (amount, currency, merchant, date, card number).
- SQLite editor — reads data from the database file, inserts the new data (expenses) into the database file.
- Finding a suitable values — find a suitable name and category by description for the expense in the database (e.g.,
  expense with the same description in the database) to provide additional context for the LLM, if the agent can't find
  exact matches.
- Memorizing — keep track of matching kinds of expenses and categories, card numbers and bank accounts, specific cases,
  etc. to learn more and more how to handle expenses.
- Duplicate/conflict handling — before/after insert, detect duplicate entries (e.g., same expenses, same transaction
  twice) and resolve conflicts in the table.

## Architecture / project layout

Each responsibility below is its own package under `internal/`, developed and tested independently before being wired
together end-to-end:

- Only one root command to run the agent as a daemon. Flag `--config` to provide the path to the config file
- bin/ - path to test binaries
- `cmd/agent` — Cobra entrypoint; the agent runs as a single daemon, `--config` selects the config file path.
- `internal/telegram` — Telegram bot backend: receives the SQLite file and screenshots, sends questions/results back to
  the user.
- `internal/llmparser` — LLM prompts construction and parsing logic: sends screenshots to the local LLM and returns
  structured `Expense` data; also resolves ambiguous name/categories via LLM comparison.
- `internal/memory` — Postgres-backed agent memory: learned name/category/description mappings, bank account mappings,
  similarity search over past decisions.
- `internal/handler` — Enrichment loop: finds a suitable name and category using memory/LLM/SQLite lookups, decides when
  a user question is required, aggregates results into the final approval message.
- `internal/store` — SQLite access: inserting parsed expenses, duplicate/conflict detection before and after insert.
- `internal/config` — YAML + Viper configuration loading (LLM, memory, telegram sections).
- something else if it can be helpful

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

## Data schemas (parsing)

LLM input: screenshots of SMS messages, in varying formats.

Expected raw data from LLM:

- type - TransactionType
- amount - float64
- currency - string
- bank_account - string
- date - string
- description - string
- parse_failed - bool
- failure_reason - string, omitempty

Edge cases handled: normal expense, refund, failed parse, transfer between accounts, income.

## Enrichment Process

The agent enriches each raw transaction with data from memory and the SQLite database, processing transactions one by
one (not batched). Enrichment prepares a transaction for insertion into the SQLite database. During this process, the
agent also learns new information and persists it to memory.

### SQLite tables used for enrichment and learning

- `associated_titles` — maps expense title to category FK.
- `categories` — maps category name to primary key; also links subcategories to their main category.
- `transactions` — main transactions table.
- `wallets` — maps wallet name to key.

### Raw SMS data (before enrichment)

```
type Expense struct {
    Type TransactionType `json:"type"`      // Classifies the transaction, see TransactionType constants.
    Amount float64 `json:"amount"`          // Transaction amount as printed in the SMS (always positive).
    Currency string `json:"currency"`       // ISO 4217 currency code, e.g. "USD", "RUB".
    Account string `json:"bank_account"`    // Bank account/card identifier from the SMS (e.g. last 4 digits).
    Date string `json:"date"`               // Transaction date/time as printed, RFC3339 if determinable, else as-is.
    Description string `json:"description"` // Merchant/counterparty name, or the other account for transfers.
    ParseFailed bool `json:"parse_failed"`  // True if the model could not confidently extract data from the image.
    FailureReason string `json:"failure_reason,omitempty"` // Why parsing failed; empty when ParseFailed is false.
}
```

### Enriched expense (after enrichment)

```
type EnrichedExpense struct {
    // FK marking which transaction is paired with another. Used only for wallet-to-wallet transfers.
    PairedTransactionFK *string `db:"paired_transaction_fk"`
    // Transaction name. Look up in sqlite "associated_titles" (by title) or agent memory first.
    // If no exact match, resolve via LLM (ResolveDescription) or user input (ResolveWithUserInput).
    Name *string `db:"name"`
    // Transaction amount. Expenses are always negative, income is positive.
    Amount float64 `db:"amount"`
    // Raw merchant/counterparty description from the SMS data.
    Note *string `db:"note"`
    // Category FK. Look up in sqlite "categories" table or agent memory.
    CategoryFK *string `db:"category_fk"`
    // Subcategory FK. Look up in sqlite "categories" table or agent memory.
    SubCategoryFK *string `db:"sub_category_fk"`
    // Wallet FK from the sqlite "wallets" table.
    WalletFK *string `db:"wallet_fk"`
    // Date the transaction was completed (timestamp format, no seconds).
    DateCreated int64 `db:"date_created"`
    // Date the agent handled the transaction (timestamp format, no seconds).
    DateTimeModified int64 `db:"date_time_modified"`
    // Date from the matching/similar transaction in the sqlite database.
    OriginalDateDue *int64 `db:"original_date_due"`
    // Boolean as int (0 or 1).
    Income *int64 `db:"income"`
    // Period length in units defined by Reoccurrence (days, weeks, months, etc.).
    PeriodLength *int64 `db:"period_length"`
    // Reoccurrence key. Must be resolved from the sqlite database.
    Reoccurrence *int64 `db:"reoccurrence"`
    EndDate                         *int64  `db:"end_date"`
    UpcomingTransactionNotification *int64  `db:"upcoming_transaction_notification"`
    Type                            *int64  `db:"type"`
    // 1 if the transaction was completed; 0 if it's a future transaction.
    Paid *int64 `db:"paid"`
    CreatedAnotherFutureTransaction *int64  `db:"created_another_future_transaction"`
    SkipPaid                        *int64  `db:"skip_paid"`
    MethodAdded                     *int64  `db:"method_added"`
    TransactionOwnerEmail           *string `db:"transaction_owner_email"`
    TransactionOriginalOwnerEmail   *string `db:"transaction_original_owner_email"`
    SharedKey                       *string `db:"shared_key"`
    SharedOldKey                    *string `db:"shared_old_key"`
    SharedStatus                    *int64  `db:"shared_status"`
    SharedDateUpdated               *int64  `db:"shared_date_updated"`
    SharedReferenceBudgetPK         *string `db:"shared_reference_budget_pk"`
    ObjectiveFK                     *string `db:"objective_fk"`
    ObjectiveLoanFK                 *string `db:"objective_loan_fk"`
    BudgetFksExclude                *string `db:"budget_fks_exclude"`
}

```

All uncommented keys must be obtained from the SQLite database.

## Agent Memory (PostgreSQL)

The agent continuously learns from the user's financial transactions — wallets, categories, and subcategories — and
persists this knowledge as structured mappings in its memory. Before a transaction is inserted into the database, the
agent uses this memory to enrich it with additional derived fields, resolving details the raw source data doesn't
provide.

Memory is backed by a PostgreSQL instance. On startup, the agent checks that the memory database is reachable and
creates any required tables that don't yet exist. It must never wipe or overwrite existing data in the process.

### Table: wallet_mappings

```
type WalletMappings struct {
    AccountPattern string `db:"account_pattern"` // pattern from raw SMS, e.g. *0746, **3421, 190-126240-52
    Name           string `db:"name"`            // must match "name" in sqlite "wallets" table
    Currency       string `db:"currency"`  // wallet's main currency; other currencies possible but unlikely
    Type           string `db:"type"`      // "bank" or "cash"
    WalletFK       string `db:"wallet_fk"` // FK to sqlite "wallets" table
}
```

### Table: transaction_mappings

```
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
