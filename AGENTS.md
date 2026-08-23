# Expense Agent

## Project goal

A Go agent that automates personal expense tracking. It runs on a local machine and ties together: an LLM for parsing, a
local SQLite database, Telegram bot (as a source of data about expenses).

## Core responsibilities

- **Telegram bot** — agent connected to Telegram bot API, receives an SQLite data file, screenshots of bank SMS from a
  user, sends questions or results back to the user.
- **LLM parsing** — send raw screenshots to a local LLM (via llama.cpp OpenAI-compatible API) and get back structured
  expense data (amount, currency, merchant, date, category, etc.).
- **SQLite editor** — insert parsed data (expenses) into the database file.
- **Finding a suitable topic** — find a suitable topic for the expense in the database (e.g., expense with the same
  description in the database) to provide additional context for the LLM, if the agent can't find exact matches.
- **Memorizing** — keep track of matching kinds of expenses and categories, card numbers and bank accounts, specific
  cases, etc. to learn more and more how to handle expenses.
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
- External services: Telegram Bot API
- Agent memory: Postgres database
- OS: Ubuntu 26.04

Frameworks:

- CMD: cobra
- Tests: gomega amd ginkgo
- Logging: zerolog
- Config: yaml and viper

## Architecture / project layout

- Only one root command to run the agent as a daemon. Flag `--config` to provide path to the config file
- bin/ - path to test binaries
- cmd/agent/ - main entrypoint
- internal/telegram/ - Telegram bot backend
- internal/LLMparser/ - LLM prompt and parsing logic
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
- Secrets (Telegram bot token, LLM API key) and other parameters live in a YAML config file, never hardcoded or
  committed.

## LLM parsing notes

- Input: screenshots of SMS'es. It can be different formats.
- Output raw JSON schema: {amount, currency, bank account, date, topic}
- Edge cases handled: usual expanse, refunds, failed parses, transfer between accounts, income

## Known constraints / things to remember

- LLM must parse images to extract SMS data about expenses.
- SQLite file must be received from the user before adding new expenses.
- After changing an SQLite file and getting a user approval, the agent must return the updated SQLite file to the user
  in a Telegram message.

## The main loop

1. The user sends an SQLite database file to the telegram bot.
2. The agent receives the file and checks it. If it isn't an SQLite, the agent returns an error message about it. If
   it's an SQLite, the agent saves the file to the data directory.
3. The agent sends a message about waiting for screenshots from the user. On this step the user can send screenshots as
   a group (multiple in one message) or one image at a time (step-by-step).
4. When the user uploads all images, he enters `/parse` command. It means the agent can start parsing.
5. Received screenshots must be saved to a queue. Every image from the queue must be parsed one by one.
6. The agent aggregates answers from llm (raw sms data) to one list.
7. The agent attempts to enrich the data to bring it to the expected structure. To do this, the agent finds exact
   matches in memory (search topic and category by description, search wallet by card number). If exact matches are
   found, the agent brings the data to the expected structure. Every element is handled one by one.
8. If the agent doesn't find something, it attempts to find matches in the SQLite data file. If the agent didn't find
   exact matches, it gets several best matches (fuzzy/substring) and exercises a request to llm.
9. Llm attempts to compare received options from the request to define suitable title, category, and wallet. If it's
   success, the agent enriches the element and saves new data to the memory.
10. If llm didn't find really suitable options (not sure more than 90%), it prepares a message with a question about
    this expense. (What is a topic and a category of this description, or what is a wallet name of this bank number.)
    One unknown expense is one request to the user. The questions are asked gradually so that the user can answer them
    calmly.
11. The user answers with a topic, and category (e.g. "poppy coffee, coffee"), or wallet name (if an question about
    wallets).
12. The agent receives user answers, enriches unknown expense using these answers, saves new information (matches) into
    the memory, and checks unknown expenses with the new saved data from the user (e.g., There are several expenses from
    one unknown place. After the user answers about the first expense, the next expenses become known. Don't need to ask
    again).
13. When the list of all parsed expenses is prepared and enriched, the agent sends it to the user and asks an approval
    (the message contains examples of answer). If the approval is received, the agent adds information to the SQLite
    database file.
14. If not, the agent saves the list as unapproved data, to allow the user to edit/fix it.
15. If the SQLite database updated successfully, the agent sends the updated file to the user. The cycle is complete.
16. To repeat this cycle the user must send a new SQLite database file.