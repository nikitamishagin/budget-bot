// Package telegram implements the agent's Telegram bot backend: it receives screenshots of bank
// SMS from allowed users, runs them through the LLM parser and memory-backed enrichment, asks the
// user for any missing information, and requests an approval before the resulting expense is
// considered ready to be saved.
package telegram

import (
	"context"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/nikitamishagin/budget-bot/internal/store"
)

// ImageParser is the subset of *llmparser.Client the bot depends on.
type ImageParser interface {
	ParseImageBytes(ctx context.Context, imageData []byte, mimeType string) ([]llmparser.Expense, error)
}

// Enricher is the subset of *handler.Enricher the bot depends on.
type Enricher interface {
	Enrich(ctx context.Context, exp llmparser.Expense, db ...handler.DBLookup) (handler.Result, error)
	ResolveWithUserInput(ctx context.Context, exp llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, income bool, db ...handler.DBLookup) (*handler.EnrichedExpense, error)
}

// ExpenseWriter is the subset of *store.Store used to find fallback enrichment candidates and
// persist an approved expense directly in the user's uploaded SQLite database file.
type ExpenseWriter interface {
	handler.DBLookup
	ResolveCategory(ctx context.Context, input string) (categoryPK string, isSubcategory bool, found bool, err error)
	ResolveWallet(ctx context.Context, input string) (walletPK string, found bool, err error)
	Insert(ctx context.Context, exp handler.EnrichedExpense) (store.InsertResult, error)
	Close() error
}

// AccountResolver is the subset of memory.Store used to turn a user-supplied wallet answer into a
// saved bank account -> wallet mapping. Mappings are keyed by (account, currency) since a single
// card can produce expenses in several currencies that must map to different wallets.
type AccountResolver interface {
	ResolveWallet(ctx context.Context, accountPattern, currency string) (memory.WalletMappings, bool, error)
	SaveWalletMapping(ctx context.Context, mapping memory.WalletMappings) error
}

// Sender is the subset of *tgbotapi.BotAPI the bot depends on, kept minimal for testability.
type Sender interface {
	SendMessage(chatID int64, text string) error
	DownloadPhoto(fileID string) ([]byte, error)
	DownloadDocument(fileID string) ([]byte, error)
	SendDocument(chatID int64, filename string, data []byte, caption string) error
}
