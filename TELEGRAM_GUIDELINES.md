# Expense Agent Telegram Guidelines

## Purpose

The agent acts as a backend of a telegram bot that allows communicating with the user through a telegram channel. It
receives screenshots of bank sms, it asks additional information from the user (if needed), it shows a result of
handling expenses, and requests an approval before saving changes in Google Drive storage.

## Details

- Get bot token from the config file
- Path for saving SQLite database files from the config file
- Inform user about the processing of requests
- Prepare messages with a request with llm (partly implemented)
- Answer on simple user requests (commands)
- Handle only requests from defined users (the list of nicknames is defined in the config file)

## Command

- /start - print a welcome message with brief instruction.
- can be extended if something needs

## Common communication flow

1. The user sends `/start` to the bot (it's needed only for the first interaction).
2. The agent responds with a welcome message and brief instruction.
3. The user sends an SQLite database file for processing.
4. The agent saves the file and asks screenshots of expenses.
5. The user sends screenshots of bank SMS about expenses (or deposit, withdrawal, etc.).
6. The agent processes the screenshots and asks for additional information if needed.
7. The agent shows a result of handling expenses as a detailed list of new expenses.
8. The user approves or rejects the changes.
9. If the approval is granted, the agent returns an updated SQLite database file to the user.
10. If the rejection is granted, the agent asks the user for help.
