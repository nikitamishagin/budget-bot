package telegramtest_test

import (
	"context"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/nikitamishagin/budget-bot/internal/store"
	"github.com/nikitamishagin/budget-bot/internal/telegram"
	"github.com/nikitamishagin/budget-bot/internal/telegram/telegramtest"
)

func TestTelegramtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Telegramtest Suite")
}

type localParser struct{}

func (localParser) ParseImageBytes(context.Context, []byte, string) ([]llmparser.Expense, error) {
	return []llmparser.Expense{{Description: "Cafe Luna", Amount: 4.5, Currency: "USD", Account: "*1234"}}, nil
}

// localEnricher asks about an unknown merchant until the user has answered.
type localEnricher struct{}

func (localEnricher) Enrich(_ context.Context, exp llmparser.Expense, _ ...handler.DBLookup) (handler.Result, error) {
	return handler.Result{NeedsUserInput: true, Kind: handler.QuestionName, Question: handler.NameQuestion(exp.Description)}, nil
}

func (localEnricher) ResolveWithUserInput(_ context.Context, exp llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, _ bool, _ ...handler.DBLookup) (*handler.EnrichedExpense, error) {
	return &handler.EnrichedExpense{Name: &name, CategoryFK: &categoryFK, WalletFK: &walletFK, Amount: -exp.Amount}, nil
}

type localAccounts struct{}

func (localAccounts) ResolveWallet(context.Context, string, string) (memory.WalletMappings, bool, error) {
	return memory.WalletMappings{WalletFK: "wallet-main"}, true, nil
}
func (localAccounts) SaveWalletMapping(context.Context, memory.WalletMappings) error { return nil }

type localWriter struct {
	mu       sync.Mutex
	inserted []handler.EnrichedExpense
}

func (*localWriter) FindCandidates(context.Context, string, int) ([]handler.DBCandidate, error) {
	return nil, nil
}
func (*localWriter) FindSimilar(context.Context, string) (handler.DBSimilarFields, bool, error) {
	return handler.DBSimilarFields{}, false, nil
}
func (*localWriter) ResolveCategory(_ context.Context, in string) (string, bool, bool, error) {
	return in, false, true, nil
}
func (*localWriter) ResolveWallet(_ context.Context, in string) (string, bool, error) {
	return in, true, nil
}
func (*localWriter) CategoryName(_ context.Context, pk string) (string, bool, error) {
	return pk, true, nil
}
func (*localWriter) WalletName(_ context.Context, pk string) (string, bool, error) {
	return pk, true, nil
}
func (w *localWriter) Insert(_ context.Context, exp handler.EnrichedExpense) (store.InsertResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inserted = append(w.inserted, exp)
	return store.InsertResult{TransactionPK: "pk"}, nil
}
func (*localWriter) Close() error { return nil }

var _ = Describe("telegramtest.User", func() {
	var (
		ctx    = context.Background()
		sender *telegramtest.FakeSender
		writer *localWriter
		bot    *telegram.Bot
	)

	BeforeEach(func() {
		sender = telegramtest.NewFakeSender()
		writer = &localWriter{}
		bot = telegram.NewBot(sender, localParser{}, localEnricher{}, localAccounts{}, []string{"alice"}, GinkgoT().TempDir())
		bot.OpenDatabase = func(string) (telegram.ExpenseWriter, error) { return writer, nil }
	})

	It("drives the full flow from database upload to the returned document", func() {
		user := telegramtest.NewUser(bot, sender, 7, "alice")

		user.SendDatabase(ctx, "expenses.sqlite", []byte("db-bytes"))
		Expect(user.LastReply()).To(ContainSubstring("Database saved"))

		user.SendPhoto(ctx, []byte("img"), "image/png")
		Expect(user.LastReply()).To(ContainSubstring("1 screenshot"))

		user.SendCommand(ctx, "/parse")
		user.SendText(ctx, "Coffee Luna")
		user.SendText(ctx, "cat-coffee")
		user.SendText(ctx, "-")
		Expect(user.LastReply()).To(ContainSubstring("Save these transactions?"))

		user.SendText(ctx, "yes")
		Expect(writer.inserted).To(HaveLen(1))
		Expect(writer.inserted[0].Name).To(HaveValue(Equal("Coffee Luna")))

		doc, ok := user.LastDocument()
		Expect(ok).To(BeTrue())
		Expect(doc.Filename).NotTo(BeEmpty())
		Expect(doc.Data).To(Equal([]byte("db-bytes")))
		Expect(len(user.Replies())).To(BeNumerically(">", 5))
	})

	It("ignores unauthorized users", func() {
		mallory := telegramtest.NewUser(bot, sender, 8, "mallory")

		mallory.SendDatabase(ctx, "expenses.sqlite", []byte("db"))
		mallory.SendCommand(ctx, "start")

		Expect(mallory.Replies()).To(BeEmpty())
		Expect(mallory.LastReply()).To(BeEmpty())
		_, ok := mallory.LastDocument()
		Expect(ok).To(BeFalse())
	})

	It("errors on unknown file ids", func() {
		_, err := sender.DownloadPhoto("nope")
		Expect(err).To(HaveOccurred())
	})
})
