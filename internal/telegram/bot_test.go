package telegram

import (
	"context"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
)

func TestTelegram(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Telegram Suite")
}

// fakeSender records sent messages instead of talking to the real Telegram API.
type fakeSender struct {
	messages  []string
	photo     []byte
	document  []byte
	sentDocs  int
	lastDocID string
}

func (f *fakeSender) SendMessage(_ int64, text string) error {
	f.messages = append(f.messages, text)
	return nil
}

func (f *fakeSender) DownloadPhoto(_ string) ([]byte, error) {
	return f.photo, nil
}

func (f *fakeSender) DownloadDocument(_ string) ([]byte, error) {
	return f.document, nil
}

func (f *fakeSender) SendDocument(_ int64, filename string, _ []byte, _ string) error {
	f.sentDocs++
	f.lastDocID = filename
	return nil
}

func (f *fakeSender) last() string {
	if len(f.messages) == 0 {
		return ""
	}
	return f.messages[len(f.messages)-1]
}

// newTestBot creates a Bot backed by a temporary directory for database files, and immediately
// feeds it a fake database upload for chat 1 so tests that focus on screenshot/enrichment
// behavior don't need to repeat that setup.
func newTestBot(sender Sender, parser ImageParser, enricher Enricher, accounts AccountResolver, allowedUsernames []string) *Bot {
	dir, err := os.MkdirTemp("", "telegram-test-*")
	if err != nil {
		panic(err)
	}
	bot := NewBot(sender, parser, enricher, accounts, allowedUsernames, dir)
	bot.HandleDocument(1, "expenses.sqlite", []byte("fake-db"))
	return bot
}

// fakeParser is a scripted ImageParser for tests.
type fakeParser struct {
	expenses []llmparser.Expense
	err      error
}

func (f *fakeParser) ParseImageBytes(_ context.Context, _ []byte, _ string) ([]llmparser.Expense, error) {
	return f.expenses, f.err
}

// fakeEnricher is a scripted Enricher for tests.
type fakeEnricher struct {
	result       handler.Result
	err          error
	resolveCalls int
	resolveArgs  []string
}

func (f *fakeEnricher) Enrich(_ context.Context, _ llmparser.Expense) (handler.Result, error) {
	return f.result, f.err
}

func (f *fakeEnricher) ResolveWithUserInput(_ context.Context, _ llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, income bool) (*handler.EnrichedExpense, error) {
	f.resolveCalls++
	f.resolveArgs = []string{name, categoryFK, subCategoryFK, walletFK}
	return &handler.EnrichedExpense{Name: name, CategoryFK: categoryFK, WalletFK: walletFK, Income: income}, nil
}

// fakeAccounts is an in-memory AccountResolver for tests.
type fakeAccounts struct {
	wallets map[string]string
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{wallets: map[string]string{}}
}

func (f *fakeAccounts) ResolveAccount(_ context.Context, accountPattern string) (string, bool, error) {
	w, ok := f.wallets[accountPattern]
	return w, ok, nil
}

func (f *fakeAccounts) SaveAccountMapping(_ context.Context, accountPattern, walletFK string) error {
	f.wallets[accountPattern] = walletFK
	return nil
}

var _ = Describe("Bot.IsAllowed", func() {
	It("only allows configured usernames, case-insensitively and with or without \"@\"", func() {
		bot := newTestBot(&fakeSender{}, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"Alice"})

		Expect(bot.IsAllowed("alice")).To(BeTrue())
		Expect(bot.IsAllowed("@alice")).To(BeTrue())
		Expect(bot.IsAllowed("bob")).To(BeFalse())
	})

	It("rejects everyone when no usernames are configured", func() {
		bot := newTestBot(&fakeSender{}, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), nil)

		Expect(bot.IsAllowed("alice")).To(BeFalse())
	})

	It("rejects an empty username", func() {
		bot := newTestBot(&fakeSender{}, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		Expect(bot.IsAllowed("")).To(BeFalse())
	})
})

var _ = Describe("Bot.HandleCommand", func() {
	It("replies with the welcome message for /start", func() {
		sender := &fakeSender{}
		bot := newTestBot(sender, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandleCommand(1, "start")

		Expect(sender.last()).To(ContainSubstring("Welcome"))
	})

	It("replies with a fallback message for unknown commands", func() {
		sender := &fakeSender{}
		bot := newTestBot(sender, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandleCommand(1, "whatever")

		Expect(sender.last()).To(ContainSubstring("Unknown command"))
	})
})

var _ = Describe("Bot.HandlePhoto", func() {
	It("asks for approval once the expense is fully enriched", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: "Starbucks", Amount: 9.5, CategoryFK: "cat-food", WalletFK: "wallet-main"}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Starbucks", Amount: 9.5}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})

		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		Expect(sender.last()).To(ContainSubstring("Save this transaction?"))
		Expect(sender.last()).To(ContainSubstring("Starbucks"))
	})

	It("relays the clarification question when the enricher needs user input", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{NeedsUserInput: true, Question: "Which wallet does account \"1234\" belong to?"}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Unknown Shop", Account: "1234"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})

		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		Expect(sender.last()).To(ContainSubstring("wallet"))
	})

	It("informs the user when the screenshot cannot be parsed", func() {
		sender := &fakeSender{}
		parser := &fakeParser{err: assertError{}}
		bot := newTestBot(sender, parser, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		Expect(sender.last()).To(ContainSubstring("could not read"))
	})
})

var _ = Describe("Bot.HandleText", func() {
	It("prompts to send a screenshot when there is no pending question", func() {
		sender := &fakeSender{}
		bot := newTestBot(sender, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandleText(context.Background(), 1, "hello")

		Expect(sender.last()).To(ContainSubstring("Send me a screenshot"))
	})

	It("re-runs enrichment after the user names the wallet for an unknown account", func() {
		sender := &fakeSender{}
		accounts := newFakeAccounts()
		enricher := &fakeEnricher{result: handler.Result{NeedsUserInput: true, Question: "Which wallet does account \"1234\" belong to?"}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Unknown Shop", Account: "1234"}}}
		bot := newTestBot(sender, parser, enricher, accounts, []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		enricher.result = handler.Result{Expense: &handler.EnrichedExpense{Name: "Unknown Shop", CategoryFK: "cat-x", WalletFK: "wallet-main"}}
		bot.HandleText(context.Background(), 1, "wallet-main")

		Expect(accounts.wallets["1234"]).To(Equal("wallet-main"))
		Expect(sender.last()).To(ContainSubstring("Save this transaction?"))
	})

	It("calls ResolveWithUserInput after the user names the category", func() {
		sender := &fakeSender{}
		accounts := newFakeAccounts()
		accounts.wallets["1234"] = "wallet-main"
		enricher := &fakeEnricher{result: handler.Result{NeedsUserInput: true, Question: "What category and wallet should I use for \"Unknown Shop\"?"}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Unknown Shop", Account: "1234"}}}
		bot := newTestBot(sender, parser, enricher, accounts, []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		bot.HandleText(context.Background(), 1, "cat-groceries")

		Expect(enricher.resolveCalls).To(Equal(1))
		Expect(enricher.resolveArgs[1]).To(Equal("cat-groceries"))
		Expect(enricher.resolveArgs[3]).To(Equal("wallet-main"))
		Expect(sender.last()).To(ContainSubstring("Save this transaction?"))
	})

	It("sends the database file back once the user approves", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: "Starbucks"}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		bot.HandleText(context.Background(), 1, "yes")

		Expect(sender.sentDocs).To(Equal(1))
	})

	It("discards the expense once the user declines", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: "Starbucks"}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		bot.HandleText(context.Background(), 1, "no")

		Expect(sender.last()).To(Equal("Discarded."))
	})

	It("re-asks when the approval answer is not yes/no", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: "Starbucks"}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		bot.HandleText(context.Background(), 1, "maybe")

		Expect(sender.last()).To(ContainSubstring("yes"))
	})
})

var _ = Describe("Bot.HandleDocument", func() {
	It("blocks screenshots until a database file has been received", func() {
		sender := &fakeSender{}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Starbucks"}}}
		dir, err := os.MkdirTemp("", "telegram-test-*")
		Expect(err).NotTo(HaveOccurred())
		bot := NewBot(sender, parser, &fakeEnricher{}, newFakeAccounts(), []string{"alice"}, dir)

		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		Expect(sender.last()).To(ContainSubstring("send your SQLite database file first"))
	})

	It("unlocks screenshot processing once a database file is saved", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: "Starbucks"}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Topic: "Starbucks"}}}
		dir, err := os.MkdirTemp("", "telegram-test-*")
		Expect(err).NotTo(HaveOccurred())
		bot := NewBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"}, dir)

		bot.HandleDocument(1, "expenses.sqlite", []byte("fake-db"))
		Expect(sender.last()).To(ContainSubstring("Database saved"))

		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		Expect(sender.last()).To(ContainSubstring("Save this transaction?"))
	})
})

// assertError is a minimal error implementation for tests.
type assertError struct{}

func (assertError) Error() string { return "boom" }
