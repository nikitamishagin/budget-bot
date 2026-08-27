package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
)

const welcomeMessage = `Welcome to the expense agent bot!

First, send me your SQLite expense database file. Once I have it, send screenshots of bank SMS about expenses - one at a time or several at once - and then send /parse when you are done, so I can process them all together, figure out categories and wallets, and ask you to confirm before saving.

Commands:
/start - show this message
/parse - process every screenshot sent so far`

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
	DBDir string
	// OpenDatabase opens the SQLite database file uploaded by a chat; defaults to openStore, and
	// is a field mainly, so tests can substitute a fake ExpenseWriter.
	OpenDatabase func(path string) (ExpenseWriter, error)
	sessions     *sessions
	databases    *databases
	screenshots  *screenshots
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
		OpenDatabase:     openStore,
		sessions:         newSessions(),
		databases:        newDatabases(),
		screenshots:      newScreenshots(),
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
func (b *Bot) HandleCommand(ctx context.Context, chatID int64, command string) {
	switch command {
	case "start":
		b.reply(chatID, welcomeMessage)
	case "parse":
		b.HandleParse(ctx, chatID)
	default:
		b.reply(chatID, "Unknown command. Send /start for instructions.")
	}
}

// HandleDocument saves an uploaded SQLite database file for the chat and unlocks screenshot
// processing, per TELEGRAM_GUIDELINES.md step 3-4 of the common communication flow. If the file
// is not a valid Cashew SQLite database, it is rejected with an error message, per AGENTS.md step
// 2 ("If it isn't an SQLite, the agent returns an error message about it").
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

	if db, err := b.OpenDatabase(path); err != nil {
		log.Warn().Err(err).Int64("chat_id", chatID).Msg("rejected non-Cashew-SQLite document from user")
		_ = os.Remove(path)
		b.reply(chatID, "That does not look like a valid SQLite expense database. Please send a valid file.")
		return
	} else {
		_ = db.Close()
	}

	b.databases.set(chatID, path)
	log.Info().Int64("chat_id", chatID).Str("path", path).Msg("saved database file from user")
	b.reply(chatID, "Database saved. Now send me screenshots of your bank SMS about expenses.")
}

// HandlePhoto queues a downloaded screenshot for later processing, per AGENTS.md steps 3-5
// ("received screenshots must be saved to a queue [...] every image from the queue must be
// parsed one by one"). Screenshots are not parsed until the user sends /parse.
func (b *Bot) HandlePhoto(_ context.Context, chatID int64, imageData []byte, mimeType string) {
	if _, ok := b.databases.get(chatID); !ok {
		b.reply(chatID, "Please send your SQLite database file first, then I can process screenshots.")
		return
	}

	n := b.screenshots.add(chatID, imageData, mimeType)
	log.Info().Int64("chat_id", chatID).Int("queued", n).Msg("queued screenshot for later parsing")
	b.reply(chatID, fmt.Sprintf("Got it (%d screenshot(s) queued). Send more, or /parse when you are done.", n))
}

// HandleParse parses every screenshot queued so far, aggregates the resulting expenses into a
// single list, and starts enriching them one by one, per AGENTS.md steps 4-6.
func (b *Bot) HandleParse(ctx context.Context, chatID int64) {
	if _, ok := b.databases.get(chatID); !ok {
		b.reply(chatID, "Please send your SQLite database file first, then I can process screenshots.")
		return
	}

	queued := b.screenshots.take(chatID)
	if len(queued) == 0 {
		b.reply(chatID, "No screenshots to parse yet. Send me some first.")
		return
	}

	b.reply(chatID, fmt.Sprintf("Parsing %d screenshot(s)...", len(queued)))

	var all []llmparser.Expense
	for _, shot := range queued {
		expenses, err := b.Parser.ParseImageBytes(ctx, shot.data, shot.mime)
		if err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to parse screenshot")
			b.reply(chatID, "Sorry, I could not read one of the screenshots. Skipping it.")
			continue
		}
		all = append(all, expenses...)
	}

	if len(all) == 0 {
		b.reply(chatID, "I could not find any transaction in the queued screenshots.")
		return
	}

	log.Info().Int64("chat_id", chatID).Int("count", len(all)).Msg("parsed expenses from queued screenshots")
	b.sessions.set(chatID, &pendingState{queue: all})
	b.processNext(ctx, chatID)
}

// HandleText processes a plain text reply from the chat, feeding it into whatever the bot is
// currently waiting for (a wallet name, a category name, a batch save approval, or an edit to a
// rejected batch).
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
	case pendingName:
		b.handleNameAnswer(chatID, state, text)
	case pendingCategory:
		b.handleCategoryAnswer(chatID, state, text)
	case pendingSubCategory:
		b.handleSubCategoryAnswer(ctx, chatID, state, text)
	case pendingApproval:
		b.handleApprovalAnswer(ctx, chatID, state, text)
	case pendingEditing:
		b.handleEditingAnswer(ctx, chatID, state, text)
	}
}

func (b *Bot) handleWalletAnswer(ctx context.Context, chatID int64, state *pendingState, walletFK string) {
	account := strings.TrimSpace(state.expense.Account)
	if account != "" {
		if err := b.Accounts.SaveAccountMapping(ctx, account, state.expense.Currency, walletFK); err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to save account mapping")
			b.reply(chatID, "Sorry, I could not save that. Please try again.")
			return
		}
	}
	// Put the current expense back at the front of the queue, so it is re-enriched now that its
	// account -> wallet mapping is known.
	state.queue = append([]llmparser.Expense{state.expense}, state.queue...)
	b.resumeQueue(ctx, chatID, state)
}

// handleNameAnswer records the user-supplied expense name and moves on to asking for the
// category, per the requirement to request name, category, and subcategory separately.
func (b *Bot) handleNameAnswer(chatID int64, state *pendingState, name string) {
	state.name = name
	state.kind = pendingCategory
	b.sessions.set(chatID, state)
	b.reply(chatID, handler.CategoryQuestion(state.expense.Description))
}

// handleCategoryAnswer records the user-supplied category and moves on to asking for the
// (optional) subcategory.
func (b *Bot) handleCategoryAnswer(chatID int64, state *pendingState, categoryFK string) {
	state.categoryFK = categoryFK
	state.kind = pendingSubCategory
	b.sessions.set(chatID, state)
	b.reply(chatID, handler.SubCategoryQuestion(state.expense.Description))
}

// handleSubCategoryAnswer records the (optional) subcategory answer - "-" leaves it empty - and
// resolves the fully-answered expense (name, category, subcategory, wallet) into the batch.
func (b *Bot) handleSubCategoryAnswer(ctx context.Context, chatID int64, state *pendingState, subCategoryFK string) {
	if subCategoryFK == "-" {
		subCategoryFK = ""
	}

	walletFK := ""
	if account := strings.TrimSpace(state.expense.Account); account != "" && b.Accounts != nil {
		if wallet, ok, err := b.Accounts.ResolveAccount(ctx, account, state.expense.Currency); err == nil && ok {
			walletFK = wallet
		}
	}
	if walletFK == "" {
		b.sessions.clear(chatID)
		b.reply(chatID, "I no longer know which wallet to use. Please send the screenshot again.")
		return
	}

	enriched, err := b.Enricher.ResolveWithUserInput(ctx, state.expense, state.name, state.categoryFK, subCategoryFK, walletFK, state.expense.Type == llmparser.TransactionIncome)
	if err != nil {
		log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to resolve with user input")
		b.reply(chatID, "Sorry, I could not save that. Please try again.")
		return
	}
	state.batch = append(state.batch, enriched)
	b.resumeQueue(ctx, chatID, state)
}

// resumeQueue continues enriching the remaining raw expenses in state.queue, after a wallet or
// category answer just resolved the current one.
func (b *Bot) resumeQueue(ctx context.Context, chatID int64, state *pendingState) {
	b.sessions.set(chatID, &pendingState{queue: state.queue, batch: state.batch})
	b.processNext(ctx, chatID)
}

func (b *Bot) handleApprovalAnswer(ctx context.Context, chatID int64, state *pendingState, answer string) {
	switch strings.ToLower(answer) {
	case "yes", "y", "confirm", "ok":
		log.Info().Int64("chat_id", chatID).Int("count", len(state.batch)).Msg("batch approved for saving")
		if err := b.saveBatch(ctx, chatID, state.batch); err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to save batch to database")
			b.reply(chatID, "Sorry, I could not save that to your database. Please try again.")
			return
		}
		b.sessions.clear(chatID)
		b.returnDatabase(chatID)
	case "no", "n", "cancel":
		log.Info().Int64("chat_id", chatID).Int("count", len(state.batch)).Msg("user rejected batch, saving as unapproved data for editing")
		b.sessions.set(chatID, &pendingState{kind: pendingEditing, batch: state.batch})
		b.reply(chatID, "Kept as unapproved data so you can fix it.\n\n"+formatBatch(state.batch)+
			"\n\nTo fix an entry, reply with \"edit <number> category|wallet|name <value>\" (e.g. \"edit 1 category cat-coffee\"). "+
			"Reply \"approve\" to save the list as-is, or \"discard\" to drop it entirely.")
	default:
		b.reply(chatID, "Please answer \"yes\" or \"no\".")
	}
}

// handleEditingAnswer lets the user fix individual entries of a previously rejected batch, per
// AGENTS.md step 14 ("saves the list as unapproved data, to allow the user to edit/fix it"), or
// approve/discard it once satisfied.
func (b *Bot) handleEditingAnswer(ctx context.Context, chatID int64, state *pendingState, answer string) {
	switch {
	case strings.EqualFold(answer, "approve"):
		log.Info().Int64("chat_id", chatID).Int("count", len(state.batch)).Msg("edited batch approved for saving")
		if err := b.saveBatch(ctx, chatID, state.batch); err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to save edited batch to database")
			b.reply(chatID, "Sorry, I could not save that to your database. Please try again.")
			return
		}
		b.sessions.clear(chatID)
		b.returnDatabase(chatID)
	case strings.EqualFold(answer, "discard"):
		log.Info().Int64("chat_id", chatID).Msg("user discarded unapproved batch")
		b.sessions.clear(chatID)
		b.reply(chatID, "Discarded.")
	case strings.HasPrefix(strings.ToLower(answer), "edit "):
		b.applyEdit(chatID, state, answer)
	default:
		b.reply(chatID, "Reply with \"edit <number> category|wallet|name <value>\", \"approve\", or \"discard\".")
	}
}

// applyEdit parses an "edit <number> <field> <value>" command and updates the matching entry of
// the chat's unapproved batch in place.
func (b *Bot) applyEdit(chatID int64, state *pendingState, command string) {
	parts := strings.SplitN(command, " ", 4)
	if len(parts) != 4 {
		b.reply(chatID, "Please use the format: \"edit <number> category|wallet|name <value>\".")
		return
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil || index < 1 || index > len(state.batch) {
		b.reply(chatID, fmt.Sprintf("Please give an entry number between 1 and %d.", len(state.batch)))
		return
	}
	field := strings.ToLower(parts[2])
	value := strings.TrimSpace(parts[3])
	entry := state.batch[index-1]
	switch field {
	case "category":
		entry.CategoryFK = value
	case "wallet":
		entry.WalletFK = value
	case "name":
		entry.Name = value
	default:
		b.reply(chatID, "Unknown field. Use \"category\", \"wallet\", or \"name\".")
		return
	}
	b.sessions.set(chatID, state)
	log.Info().Int64("chat_id", chatID).Int("entry", index).Str("field", field).Msg("edited unapproved batch entry")
	b.reply(chatID, "Updated.\n\n"+formatBatch(state.batch)+"\n\nReply \"approve\" to save, keep editing, or \"discard\".")
}

// saveBatch inserts every expense in a batch into the chat's SQLite database file, per AGENTS.md's
// "SQLite editor" responsibility, skipping any duplicate entries (but not treating them as an
// error) per "Duplicate/conflict handling".
func (b *Bot) saveBatch(ctx context.Context, chatID int64, batch []*handler.EnrichedExpense) error {
	path, ok := b.databases.get(chatID)
	if !ok {
		return fmt.Errorf("no database file on file for this chat")
	}

	db, err := b.OpenDatabase(path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	for _, enriched := range batch {
		result, err := db.Insert(ctx, *enriched)
		if err != nil {
			return fmt.Errorf("insert expense: %w", err)
		}
		if result.Duplicate {
			log.Info().Int64("chat_id", chatID).Str("transaction_pk", result.TransactionPK).Msg("expense already exists in database, skipped insert")
		} else {
			log.Info().Int64("chat_id", chatID).Str("transaction_pk", result.TransactionPK).Msg("inserted expense into database")
		}
	}
	return nil
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

// processNext enriches the next raw expense in the chat's pending queue, aggregating results
// into its batch, and either asks a follow-up question (setting the appropriate pendingWallet or
// pendingCategory state) or moves on to the next queued expense, per AGENTS.md steps 7-9. Once
// the queue is empty, it asks the chat to approve the whole aggregated batch, per step 13.
func (b *Bot) processNext(ctx context.Context, chatID int64) {
	state, ok := b.sessions.get(chatID)
	if !ok {
		return
	}

	for len(state.queue) > 0 {
		exp := state.queue[0]
		remaining := state.queue[1:]

		result, err := func() (handler.Result, error) {
			var dbLookups []handler.DBLookup
			if path, ok := b.databases.get(chatID); ok {
				if db, err := b.OpenDatabase(path); err == nil {
					defer db.Close()
					dbLookups = append(dbLookups, db)
				} else {
					log.Warn().Err(err).Int64("chat_id", chatID).Msg("failed to open database file for candidate lookup")
				}
			}
			return b.Enricher.Enrich(ctx, exp, dbLookups...)
		}()
		if err != nil {
			log.Error().Err(err).Int64("chat_id", chatID).Msg("failed to enrich expense")
			b.reply(chatID, "Sorry, something went wrong while processing one of the transactions. Skipping it.")
			state.queue = remaining
			continue
		}

		if result.NeedsUserInput {
			kind := pendingName
			if isWalletQuestion(result.Question) {
				kind = pendingWallet
			}
			b.sessions.set(chatID, &pendingState{kind: kind, expense: exp, queue: remaining, batch: state.batch})
			log.Info().Int64("chat_id", chatID).Str("question", result.Question).Msg("asked additional info from user")
			b.reply(chatID, result.Question)
			return
		}

		state.batch = append(state.batch, result.Expense)
		state.queue = remaining
	}

	b.askBatchApproval(chatID, state.batch)
}

// askBatchApproval asks the chat to approve saving every expense aggregated during the current
// /parse run, per AGENTS.md step 13.
func (b *Bot) askBatchApproval(chatID int64, batch []*handler.EnrichedExpense) {
	if len(batch) == 0 {
		b.sessions.clear(chatID)
		b.reply(chatID, "Nothing left to save.")
		return
	}
	b.sessions.set(chatID, &pendingState{kind: pendingApproval, batch: batch})
	log.Info().Int64("chat_id", chatID).Int("count", len(batch)).Msg("asked user for batch save approval")
	b.reply(chatID, formatBatch(batch)+"\n\nSave these transactions? (yes/no)")
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
	sub := e.SubCategoryFK
	if sub == "" {
		sub = "-"
	}
	return fmt.Sprintf(
		"%s: %s\nAmount: %.2f\nCategory: %s\nSubcategory: %s\nWallet: %s\nNote: %s\nDate: %s",
		kind, e.Name, e.Amount, e.CategoryFK, sub, e.WalletFK, e.RawDescription, e.Date,
	)
}

// formatBatch renders every expense in a batch as a numbered list, so the user can refer to an
// entry by number when editing a rejected batch (see applyEdit) or simply review it before
// approving.
func formatBatch(batch []*handler.EnrichedExpense) string {
	var b strings.Builder
	for i, e := range batch {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, formatEnrichedExpense(e))
	}
	return b.String()
}
