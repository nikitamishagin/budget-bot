package telegram

import (
	"context"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/nikitamishagin/budget-bot/internal/store"
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

// newTestBot creates a Bot backed by a temporary directory for database files, wired to a fake
// ExpenseWriter (so tests never touch a real SQLite driver), and immediately feeds it a fake
// database upload for chat 1 so tests that focus on screenshot/enrichment behavior don't need to
// repeat that setup.
func newTestBot(sender Sender, parser ImageParser, enricher Enricher, accounts AccountResolver, allowedUsernames []string) *Bot {
	dir, err := os.MkdirTemp("", "telegram-test-*")
	if err != nil {
		panic(err)
	}
	bot := NewBot(sender, parser, enricher, accounts, allowedUsernames, dir)
	bot.OpenDatabase = func(string) (ExpenseWriter, error) { return newFakeWriter(), nil }
	bot.HandleDocument(1, "expenses.sqlite", []byte("fake-db"))
	return bot
}

// fakeWriter is a stub ExpenseWriter for tests, so bot tests never need a real SQLite file.
type fakeWriter struct {
	inserted []handler.EnrichedExpense
}

func newFakeWriter() *fakeWriter { return &fakeWriter{} }

func (f *fakeWriter) FindCandidates(_ context.Context, _ string, _ int) ([]handler.DBCandidate, error) {
	return nil, nil
}

func (f *fakeWriter) FindSimilar(_ context.Context, _ string) (handler.DBSimilarFields, bool, error) {
	return handler.DBSimilarFields{}, false, nil
}

func (f *fakeWriter) Insert(_ context.Context, exp handler.EnrichedExpense) (store.InsertResult, error) {
	f.inserted = append(f.inserted, exp)
	return store.InsertResult{TransactionPK: "fake-pk"}, nil
}

func (f *fakeWriter) Close() error { return nil }

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

func (f *fakeEnricher) Enrich(_ context.Context, _ llmparser.Expense, _ ...handler.DBLookup) (handler.Result, error) {
	return f.result, f.err
}

func (f *fakeEnricher) ResolveWithUserInput(_ context.Context, _ llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, income bool, _ ...handler.DBLookup) (*handler.EnrichedExpense, error) {
	f.resolveCalls++
	f.resolveArgs = []string{name, categoryFK, subCategoryFK, walletFK}
	return &handler.EnrichedExpense{Name: &name, CategoryFK: &categoryFK, WalletFK: &walletFK, Income: boolToInt64Ptr(income)}, nil
}

// boolToInt64Ptr returns a pointer to the int64 representation of b (1 for true, 0 for false),
// used to fill the nullable Income field of handler.EnrichedExpense in tests.
func boolToInt64Ptr(b bool) *int64 {
	var v int64
	if b {
		v = 1
	}
	return &v
}

// strP builds the pointer values handler.EnrichedExpense's fields need, keeping test literals
// concise.
func strP(s string) *string { return &s }

// fakeAccounts is an in-memory AccountResolver for tests.
type fakeAccounts struct {
	wallets map[string]memory.WalletMappings
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{wallets: map[string]memory.WalletMappings{}}
}

func (f *fakeAccounts) ResolveWallet(_ context.Context, accountPattern, currency string) (memory.WalletMappings, bool, error) {
	w, ok := f.wallets[accountPattern+"|"+currency]
	return w, ok, nil
}

func (f *fakeAccounts) SaveWalletMapping(_ context.Context, mapping memory.WalletMappings) error {
	f.wallets[mapping.AccountPattern+"|"+mapping.Currency] = mapping
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

		bot.HandleCommand(context.Background(), 1, "start")

		Expect(sender.last()).To(ContainSubstring("Welcome"))
	})

	It("replies with a fallback message for unknown commands", func() {
		sender := &fakeSender{}
		bot := newTestBot(sender, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandleCommand(context.Background(), 1, "whatever")

		Expect(sender.last()).To(ContainSubstring("Unknown command"))
	})
})

var _ = Describe("Bot.HandlePhoto and Bot.HandleParse", func() {
	It("queues the screenshot instead of parsing it immediately", func() {
		sender := &fakeSender{}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks", Amount: 9.5}}}
		bot := newTestBot(sender, parser, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		Expect(sender.last()).To(ContainSubstring("queued"))
	})

	It("asks for approval once every queued screenshot is parsed and enriched", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks"), Amount: 9.5, CategoryFK: strP("cat-food"), WalletFK: strP("wallet-main")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks", Amount: 9.5}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		bot.HandleParse(context.Background(), 1)

		Expect(sender.last()).To(ContainSubstring("Save these transactions?"))
		Expect(sender.last()).To(ContainSubstring("Starbucks"))
	})

	It("relays the clarification question when the enricher needs user input", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{NeedsUserInput: true, Question: "Which wallet does account \"1234\" belong to?"}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Unknown Shop", Account: "1234"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		bot.HandleParse(context.Background(), 1)

		Expect(sender.last()).To(ContainSubstring("wallet"))
	})

	It("informs the user when a screenshot cannot be parsed and no transaction is found", func() {
		sender := &fakeSender{}
		parser := &fakeParser{err: assertError{}}
		bot := newTestBot(sender, parser, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("fake-image"), "image/jpeg")

		bot.HandleParse(context.Background(), 1)

		Expect(sender.last()).To(ContainSubstring("could not find any transaction"))
	})

	It("complains when /parse is sent with nothing queued", func() {
		sender := &fakeSender{}
		bot := newTestBot(sender, &fakeParser{}, &fakeEnricher{}, newFakeAccounts(), []string{"alice"})

		bot.HandleParse(context.Background(), 1)

		Expect(sender.last()).To(ContainSubstring("No screenshots to parse"))
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
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Unknown Shop", Account: "1234"}}}
		bot := newTestBot(sender, parser, enricher, accounts, []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)

		enricher.result = handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Unknown Shop"), CategoryFK: strP("cat-x"), WalletFK: strP("wallet-main")}}
		bot.HandleText(context.Background(), 1, "wallet-main")

		Expect(accounts.wallets["1234|"].WalletFK).To(Equal("wallet-main"))
		Expect(sender.last()).To(ContainSubstring("Save these transactions?"))
	})

	It("asks for name, category, and subcategory separately, saving each into the correct field", func() {
		sender := &fakeSender{}
		accounts := newFakeAccounts()
		accounts.wallets["1234|"] = memory.WalletMappings{AccountPattern: "1234", WalletFK: "wallet-main"}
		enricher := &fakeEnricher{result: handler.Result{NeedsUserInput: true, Question: "What is the name of the \"Unknown Shop\" expense?"}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Unknown Shop", Account: "1234"}}}
		bot := newTestBot(sender, parser, enricher, accounts, []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)

		bot.HandleText(context.Background(), 1, "Corner Shop")
		Expect(sender.last()).To(ContainSubstring("category"))

		bot.HandleText(context.Background(), 1, "cat-groceries")
		Expect(sender.last()).To(ContainSubstring("subcategory"))

		bot.HandleText(context.Background(), 1, "-")

		Expect(enricher.resolveCalls).To(Equal(1))
		Expect(enricher.resolveArgs[0]).To(Equal("Corner Shop"))
		Expect(enricher.resolveArgs[1]).To(Equal("cat-groceries"))
		Expect(enricher.resolveArgs[2]).To(Equal(""))
		Expect(enricher.resolveArgs[3]).To(Equal("wallet-main"))
		Expect(sender.last()).To(ContainSubstring("Save these transactions?"))
	})

	It("sends the database file back once the user approves", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)

		bot.HandleText(context.Background(), 1, "yes")

		Expect(sender.sentDocs).To(Equal(1))
	})

	It("discards the expense once the user declines", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)

		bot.HandleText(context.Background(), 1, "no")

		Expect(sender.last()).To(ContainSubstring("unapproved data"))
	})

	It("re-asks when the approval answer is not yes/no", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)

		bot.HandleText(context.Background(), 1, "maybe")

		Expect(sender.last()).To(ContainSubstring("yes"))
	})

	It("allows editing a rejected batch and approving it afterwards", func() {
		sender := &fakeSender{}
		writer := newFakeWriter()
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks"), CategoryFK: strP("cat-wrong"), WalletFK: strP("wallet-1")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.OpenDatabase = func(string) (ExpenseWriter, error) { return writer, nil }
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)
		bot.HandleText(context.Background(), 1, "no")

		bot.HandleText(context.Background(), 1, "edit 1 category cat-coffee")
		Expect(sender.last()).To(ContainSubstring("cat-coffee"))

		bot.HandleText(context.Background(), 1, "approve")

		Expect(writer.inserted).To(HaveLen(1))
		Expect(writer.inserted[0].CategoryFK).To(HaveValue(Equal("cat-coffee")))
		Expect(sender.sentDocs).To(Equal(1))
	})

	It("discards a rejected batch entirely on request", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)
		bot.HandleText(context.Background(), 1, "no")

		bot.HandleText(context.Background(), 1, "discard")

		Expect(sender.last()).To(Equal("Discarded."))
	})
})

var _ = Describe("Bot.HandleDocument", func() {
	It("blocks screenshots until a database file has been received", func() {
		sender := &fakeSender{}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		dir, err := os.MkdirTemp("", "telegram-test-*")
		Expect(err).NotTo(HaveOccurred())
		bot := NewBot(sender, parser, &fakeEnricher{}, newFakeAccounts(), []string{"alice"}, dir)

		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")

		Expect(sender.last()).To(ContainSubstring("send your SQLite database file first"))
	})

	It("unlocks screenshot processing once a database file is saved", func() {
		sender := &fakeSender{}
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		dir, err := os.MkdirTemp("", "telegram-test-*")
		Expect(err).NotTo(HaveOccurred())
		bot := NewBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"}, dir)
		bot.OpenDatabase = func(string) (ExpenseWriter, error) { return newFakeWriter(), nil }

		bot.HandleDocument(1, "expenses.sqlite", []byte("fake-db"))
		Expect(sender.last()).To(ContainSubstring("Database saved"))

		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)
		Expect(sender.last()).To(ContainSubstring("Save these transactions?"))
	})

	It("rejects a document that is not a valid SQLite database", func() {
		sender := &fakeSender{}
		parser := &fakeParser{}
		dir, err := os.MkdirTemp("", "telegram-test-*")
		Expect(err).NotTo(HaveOccurred())
		bot := NewBot(sender, parser, &fakeEnricher{}, newFakeAccounts(), []string{"alice"}, dir)
		bot.OpenDatabase = func(string) (ExpenseWriter, error) { return nil, assertError{} }

		bot.HandleDocument(1, "not-a-db.sqlite", []byte("not sqlite at all"))

		Expect(sender.last()).To(ContainSubstring("does not look like a valid SQLite"))
	})
})

var _ = Describe("Bot approval saving", func() {
	It("inserts the enriched expense into the chat's database before returning it", func() {
		sender := &fakeSender{}
		writer := newFakeWriter()
		enricher := &fakeEnricher{result: handler.Result{Expense: &handler.EnrichedExpense{Name: strP("Starbucks"), Amount: 5.5, CategoryFK: strP("cat-coffee"), WalletFK: strP("wallet-1")}}}
		parser := &fakeParser{expenses: []llmparser.Expense{{Description: "Starbucks"}}}
		bot := newTestBot(sender, parser, enricher, newFakeAccounts(), []string{"alice"})
		bot.OpenDatabase = func(string) (ExpenseWriter, error) { return writer, nil }
		bot.HandlePhoto(context.Background(), 1, []byte("img"), "image/jpeg")
		bot.HandleParse(context.Background(), 1)

		bot.HandleText(context.Background(), 1, "yes")

		Expect(writer.inserted).To(HaveLen(1))
		Expect(writer.inserted[0].Name).To(HaveValue(Equal("Starbucks")))
		Expect(sender.sentDocs).To(Equal(1))
	})
})

// assertError is a minimal error implementation for tests.
type assertError struct{}

func (assertError) Error() string { return "boom" }
