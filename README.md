# Budget Bot

Budget Bot (Expense Agent) is a Go daemon that automates personal expense tracking. It runs locally and ties together a
Telegram bot, a local LLM, and a SQLite database to turn bank SMS screenshots into structured, categorized transactions,
ready to be saved back into the user's expense-tracking database.

Concretely, the agent:

- Receives an SQLite database file and screenshots of bank SMS messages via a Telegram bot.
- Parses the screenshots with a local LLM (via a llama.cpp OpenAI-compatible API) into structured expense data (amount,
  currency, merchant, date, card number).
- Enriches each parsed expense with the category, wallet, and name required by the database schema, using its own memory
  of previously learned mappings first, then falling back to the SQLite file and, if needed, the LLM.
- Asks the user for clarification only when it cannot confidently resolve an expense on its own, and remembers the
  answer for next time.
- Detects duplicate or conflicting entries before writing to the database, and returns the updated database file to the
  user once changes are approved.

## Project goal

The primary practical goal of this project is to create a working personal expense tracking agent that will eliminate
the need to manually enter data into an expense tracking app.

However, an equally important secondary goal is that this project is an agent programming exercise — using an LLM agent
to design, implement, test, and iteratively refine a real, non-trivial Go application from start to finish. The goal is
to gain practical experience with agent-driven development workflows (task decomposition, best-practice agreements,
iterative testing, and bug fixing).

## LLM parser

The `internal/llmparser` package sends a bank SMS screenshot to a local OpenAI-compatible vision LLM (llama.cpp server)
and returns structured expense data (amount, currency, account, date, topic, category, description).

### Configuration

Copy `config.example.yaml` to `config.yaml` and adjust as needed:

```yaml
llm:
  base_url: "http://127.0.0.1:8080/v1"
  api_key_file: "./api-key"
  model: "model_name"
  timeout_seconds: 120

log:
  level: "info"

memory:
  host: "127.0.0.1"
  port: 5432
  user: "devuser"
  password: "devpass"
  dbname: "expense_agent"
  sslmode: "disable"
  top_n: 5
  high_confidence_threshold: 0.9
telegram:
  token_file: "./telegram-bot-token"
  allowed_usernames:
    - "@nickname"
  db_path: "./data/databases"

```

### CLI usage

Build and run the agent:

```bash
go build -o agent ./cmd/agent
./agent -c config.yaml
```

After running the agent, the Telegram bot can be used to interact with the expense tracker.

```bash
docker run -d \
  --name expense-agent-db \
  --env-file ./postgres.env \
  -p 5432:5432 \
  -v expense_agent_pgdata:/var/lib/postgresql \
  postgres:18-alpine
```

## License

This project is licensed under the MIT License — see the `LICENSE` file for the full text. In short, you are free to
use, copy, modify, and distribute this software, provided the original copyright notice and license text are included.
