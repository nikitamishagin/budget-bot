# Expense Agent

## Project goal

A Go agent that automates personal expense tracking. It runs on a local machine and ties together: an LLM for parsing, a
local SQLite database, Telegram bot (as a source of data about expenses), and Google Drive (for sync/backup).

## Core responsibilities

- **Telegram bot** — agent connected to Telegram bot API, receives screenshots of bank SMS from a user, sends questions
  or results back to the user.
- **LLM parsing** — send raw screenshots to a local LLM (via llama.cpp OpenAI-compatible API) and get back structured
  expense data (amount, currency, merchant, date, category, etc.).
- **Google Drive sync** — sync the SQLite file to Google Drive (third-party backup of Cashew application).
- **SQLite editor** — insert parsed data (expenses) into the database file.
- **Finding a suitable topic** — find a suitable topic for the expense in the database or memory file (e.g., expense
  with the same description in the database).
- **Duplicate/conflict handling** — before/after insert, detect duplicate entries (e.g., same expenses, same transaction
  twice) and resolve conflicts in the table.

Each of these should be its own package under `internal/`, developed and tested independently before wiring them
together end-to-end.

## Tech stack

- Language: Go version 1.26.6
- LLM runtime: llama.cpp, OpenAI-compatible API, local inference
    - Model: Qwen3.5-9B-NVFP4-GGUF (swappable — don't hardcode model name where avoidable)
    - Hardware: RTX 5070 12GB VRAM, 16GB RAM — keep this in mind for context window / concurrency limits when designing
      LLM calls
    - Context window: 128000 tokens
- Storage: SQLite (single file)
- External services: Telegram Bot API, Google Drive API
- OS: Ubuntu 26.04

Frameworks:

- CMD: cobra
- Tests: gomega amd ginkgo
- Logging: zerolog
- Config: yaml and viper

## Architecture / project layout

- bin/ - path to test binaries
- cmd/agent/ - main entrypoint
- internal/telegram/ - Telegram bot backend
- internal/LLMparser/ - LLM prompt and parsing logic
- internal/drivesync/ - Google Drive sync
- internal/memory/ - Postgres-backed agent memory (learned topic/category/wallet mappings, bank account mappings,
  similarity search)
- internal/handler/ - Enrichment loop: find suitable topics using memory/LLM, decide when to ask the user
- internal/store/ - SQLite access, dedup/conflict logic
- something else if it can be helpful

## Conventions

- Follow Go development best practices.
- Prefer simplicity over abstraction, avoid premature interfaces/layers.
- Implement logging with several log levels.
- Write simple tests to cover critical paths.
- Use linter to enforce code quality.
- Secrets (Telegram bot token, Google OAuth credentials) and other parameters live in a YAML config file, never
  hardcoded or committed.

## LLM parsing notes

- Input: screenshots of SMS'es. It can be different formats.
- Output raw JSON schema: {amount, currency, bank account, date, topic}
- Edge cases handled: usual expanse, refunds, failed parses, transfer between accounts, income

## Known constraints / things to remember

- LLM must parse images to extract SMS data about expenses.
- SQLite file stored in Google Drive as a third-party backup of Cashew mobile app. To get or write it, it must use
  information from the Cashew GitHub project.
- Cashew mobile app GitHub repo: https://github.com/jameskokoska/Cashew
