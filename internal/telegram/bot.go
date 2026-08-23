package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
)

const welcomeMessage = `Welcome to the expense agent bot!

First, send me your SQLite expense database file. Once I have it, send screenshots of bank SMS about expenses and I will parse them, figure out the category and wallet, and ask you to confirm before saving.

Commands:
/start - show this message`

// Bot wires the Telegram frontend to the LLM parser and the memory-backed enrichment loop. It
// only reacts to messages from users listed in AllowedUsernames, per TELEGRAM_GUIDELINES.md.
type Bot struct {
	Sender           Sender
	Parser           ImageParser
	Enricher         Enricher
	Accounts         AccountResolver
	AllowedUsernames map[string]bool
	// DBDir is the directory (from config) where uploaded SQLite database files are saved, per
	// TELEGRAM_GUIDELINES.md.
	DBDir     string
	sessions  *sessions
	databases *databases
}

// NewBot creates a Bot. allowedUsernames may be empty, in which case every user is rejected -
// TELEGRAM_GUIDELINES.md requires the bot to only handle requests from users defined in config.
// Usernames are matched case-insensitively and may be given with or without a leading "@".
func NewBot(sender Sender, parser ImageParser, enricher Enricher, accounts AccountResolver, allowedUsernames []string, dbDir string) *Bot {
	allowed := make(map[string]bool, len(allowedUsernames))
	for _, name := range allowedUsernames {
		allowed[normalizeUsername(name)] = true
	}
	return &Bot{
		Sender:           sender,
		Parser:           parser,
		Enricher:         enricher,
		Accounts:         accounts,
		AllowedUsernames: allowed,
		DBDir:            dbDir,
		sessions:         newSessions(),
		databases:        newDatabases(),
	}
}

// normalizeUsername lower-cases a Telegram username and strips a leading "@", so usernames can
// be matched consistently regardless of how they were entered in the config or sent by Telegram.
func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
}

// IsAllowed reports whether the given Telegram username is allowed to talk to the bot.
func (b *Bot) IsAllowed(username string) bool {
	if username == "" {
		return false
	}
	return b.AllowedUsernames[normalizeUsername(username)]
}

// HandleCommand handles a bot command (e.g. "start") for the given chat.
func (b *Bot) HandleCommand(chatID int64, command string) {
	switch command {
	case "start":
		b.reply(chatID, welcomeMessage)
	default:
		b.reply(chatID, "Unknown command. Send /start for instructions.")
	}
}

// HandleDocument saves an uploaded SQLite database file for the chat and unlocks screenshot
// processing, per TELEGRAM_GUIDELINES.md step 3-4 of the common communication flow.
func (b *Bot) HandleDocument(chatID int64, fileName string, data []byte) {
	if err := os.MkdirAll(b.DBDir, 0o755); err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to create database directory")
		b.reply(chatID, "Sorry, I could not save that file. Please try again.")
		return
	}

	if fileName == "" {
		fileName = "database.sqlite"
	}
	path := filepath.Join(b.DBDir, fmt.Sprintf("chat-%d-%s", chatID, filepath.Base(fileName)))

	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to write database file")
		b.reply(chatID, "Sorry, I could not save that file. Please try again.")
		return
	}

	b.databases.set(chatID, path)
	log.Info().Int64("chat_id", chatID).Str("path", path).Msg("saved database file from user")
	b.reply(chatID, "Database saved. Now send me screenshots of your bank SMS about expenses.")
}

// HandlePhoto processes a downloaded screenshot: parses it with the LLM and enriches every
// expense found in it, asking the chat for missing information or approval as needed.
func (b *Bot) HandlePhoto(ctx context.Context, chatID int64, imageData []byte, mimeType string) {
	if _, ok := b.databases.get(chatID); !ok {
		b.reply(chatID, "Please send your SQLite database file first, then I can process screenshots.")
		return
	}

	b.reply(chatID, "Got it, processing the screenshot...")

	expenses, err := b.Parser.ParseImageBytes(ctx, imageData, mimeType)
	if err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to parse screenshot")
		b.reply(chatID, "Sorry, I could not read that screenshot. Please try again.")
		return
	}
	if len(expenses) == 0 {
		b.reply(chatID, "I could not find any transaction in that screenshot.")
		return
	}

	log.Info().Int64("chat_id", chatID).Int("count", len(expenses)).Msg("parsed expenses from screenshot")

	for _, exp := range expenses {
		b.enrichAndAsk(ctx, chatID, exp)
	}
}

// HandleText processes a plain text reply from the chat, feeding it into whatever the bot is
// currently waiting for (a wallet name, a category name, or a save approval).
func (b *Bot) HandleText(ctx context.Context, chatID int64, text string) {
	state, ok := b.sessions.get(chatID)
	if !ok {
		b.reply(chatID, "Send me a screenshot of a bank SMS to get started, or /start for instructions.")
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		b.reply(chatID, "Please send a non-empty answer.")
		return
	}

	switch state.kind {
	case pendingWallet:
		b.handleWalletAnswer(ctx, chatID, state, text)
	case pendingCategory:
		b.handleCategoryAnswer(ctx, chatID, state, text)
	case pendingApproval:
		b.handleApprovalAnswer(ctx, chatID, state, text)
	}
}

func (b *Bot) handleWalletAnswer(ctx context.Context, chatID int64, state *pendingState, walletFK string) {
	account := strings.TrimSpace(state.expense.Account)
	if account != "" {
		if err := b.Accounts.SaveAccountMapping(ctx, account, walletFK); err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to save account mapping")
			b.reply(chatID, "Sorry, I could not save that. Please try again.")
			return
		}
	}
	b.sessions.clear(chatID)
	b.enrichAndAsk(ctx, chatID, state.expense)
}

func (b *Bot) handleCategoryAnswer(ctx context.Context, chatID int64, state *pendingState, categoryFK string) {
	walletFK := ""
	if account := strings.TrimSpace(state.expense.Account); account != "" && b.Accounts != nil {
		if wallet, ok, err := b.Accounts.ResolveAccount(ctx, account); err == nil && ok {
			walletFK = wallet
		}
	}
	if walletFK == "" {
		b.sessions.clear(chatID)
		b.reply(chatID, "I no longer know which wallet to use. Please send the screenshot again.")
		return
	}

	enriched, err := b.Enricher.ResolveWithUserInput(ctx, state.expense, state.expense.Topic, categoryFK, "", walletFK, state.expense.Type == llmparser.TransactionIncome)
	if err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to resolve with user input")
		b.reply(chatID, "Sorry, I could not save that. Please try again.")
		return
	}
	b.askApproval(chatID, enriched)
}

func (b *Bot) handleApprovalAnswer(_ context.Context, chatID int64, state *pendingState, answer string) {
	switch strings.ToLower(answer) {
	case "yes", "y", "confirm", "ok":
		// TODO: hand off state.enriched to internal/store once the SQLite/Cashew writer and
		// dedup/conflict logic are wired in; for now, the database file is returned unmodified.
		log.Info().Int64("chat_id", chatID).Interface("expense", state.enriched).Msg("expense approved for saving")
		b.sessions.clear(chatID)
		b.returnDatabase(chatID)
	case "no", "n", "cancel":
		log.Info().Int64("chat_id", chatID).Msg("user discarded expense")
		b.sessions.clear(chatID)
		b.reply(chatID, "Discarded.")
	default:
		b.reply(chatID, "Please answer \"yes\" or \"no\".")
	}
}

// returnDatabase sends the chat's saved SQLite database file back, per TELEGRAM_GUIDELINES.md:
// "After the approval is granted, the agent returns an updated SQLite database file to the user."
func (b *Bot) returnDatabase(chatID int64) {
	path, ok := b.databases.get(chatID)
	if !ok {
		b.reply(chatID, "Saved, but I no longer have your database file on hand.")
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to read database file")
		b.reply(chatID, "Saved, but I could not read the database file to send it back.")
		return
	}

	if err := b.Sender.SendDocument(chatID, filepath.Base(path), data, "Saved. Here is your updated database."); err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send database file back")
		b.reply(chatID, "Saved, but I could not send the database file back. Please try again.")
		return
	}
	log.Info().Int64("chat_id", chatID).Msg("returned database file to user")
}

// enrichAndAsk runs the enrichment loop for a single parsed expense and either asks the chat a
// follow-up question or, once fully enriched, asks for a save approval.
func (b *Bot) enrichAndAsk(ctx context.Context, chatID int64, exp llmparser.Expense) {
	result, err := b.Enricher.Enrich(ctx, exp)
	if err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to enrich expense")
		b.reply(chatID, "Sorry, something went wrong while processing that transaction.")
		return
	}

	if result.NeedsUserInput {
		kind := pendingCategory
		if isWalletQuestion(result.Question) {
			kind = pendingWallet
		}
		b.sessions.set(chatID, &pendingState{kind: kind, expense: exp})
		log.Info().Int64("chat_id", chatID).Str("question", result.Question).Msg("asked additional info from user")
		b.reply(chatID, result.Question)
		return
	}

	b.askApproval(chatID, result.Expense)
}

func (b *Bot) askApproval(chatID int64, enriched *handler.EnrichedExpense) {
	b.sessions.set(chatID, &pendingState{kind: pendingApproval, enriched: enriched})
	log.Info().Int64("chat_id", chatID).Msg("asked user for save approval")
	b.reply(chatID, formatEnrichedExpense(enriched)+"\n\nSave this transaction? (yes/no)")
}

func (b *Bot) reply(chatID int64, text string) {
	if err := b.Sender.SendMessage(chatID, text); err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send telegram message")
	}
}

// isWalletQuestion tells apart the "unknown wallet" question the Enricher asks (see
// resolveWallet in internal/handler) from every other clarification question, so a plain text
// answer can be routed to the right follow-up action.
func isWalletQuestion(question string) bool {
	return strings.Contains(question, "does account")
}

func formatEnrichedExpense(e *handler.EnrichedExpense) string {
	kind := "Expense"
	if e.Income {
		kind = "Income"
	}
	return fmt.Sprintf(
		"%s: %s\nAmount: %.2f\nCategory: %s\nWallet: %s\nDate: %s",
		kind, e.Name, e.Amount, e.CategoryFK, e.WalletFK, e.Date,
	)
}
