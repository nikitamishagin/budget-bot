package handler

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
)

func TestHandler(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Handler Suite")
}

// fakeMemory is an in-memory MemoryStore implementation for tests.
type fakeMemory struct {
	pairs        []memory.TrainingPair
	accounts     map[string]string
	saveCalls    int
	saveAccounts int
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{accounts: map[string]string{}}
}

func (f *fakeMemory) FindBestMatch(_ context.Context, description string) (memory.Candidate, bool, error) {
	for _, p := range f.pairs {
		if p.RawDesc == description {
			return memory.Candidate{TrainingPair: p, Score: 1.0}, true, nil
		}
	}
	if len(f.pairs) == 0 {
		return memory.Candidate{}, false, nil
	}
	return memory.Candidate{TrainingPair: f.pairs[0], Score: 0.3}, true, nil
}

func (f *fakeMemory) TopCandidates(_ context.Context, _ string, n int) ([]memory.Candidate, error) {
	var out []memory.Candidate
	for i, p := range f.pairs {
		if i >= n {
			break
		}
		out = append(out, memory.Candidate{TrainingPair: p, Score: 0.5})
	}
	return out, nil
}

func (f *fakeMemory) SaveTrainingPair(_ context.Context, pair memory.TrainingPair) error {
	f.saveCalls++
	f.pairs = append(f.pairs, pair)
	return nil
}

func (f *fakeMemory) ResolveAccount(_ context.Context, accountPattern, currency string) (string, bool, error) {
	wallet, ok := f.accounts[accountPattern+"|"+currency]
	return wallet, ok, nil
}

func (f *fakeMemory) SaveAccountMapping(_ context.Context, accountPattern, currency, walletFK string) error {
	f.saveAccounts++
	f.accounts[accountPattern+"|"+currency] = walletFK
	return nil
}

// fakeResolver is a scripted Resolver for tests.
type fakeResolver struct {
	resolution llmparser.Resolution
	err        error
	calls      int
	lastCands  []llmparser.Candidate
}

func (f *fakeResolver) ResolveTopic(_ context.Context, _ llmparser.Expense, candidates []llmparser.Candidate) (llmparser.Resolution, error) {
	f.calls++
	f.lastCands = candidates
	return f.resolution, f.err
}

var _ = Describe("Enricher.Enrich", func() {
	var (
		ctx context.Context
		mem *fakeMemory
		res *fakeResolver
		enr *Enricher
		exp llmparser.Expense
	)

	BeforeEach(func() {
		ctx = context.Background()
		mem = newFakeMemory()
		res = &fakeResolver{}
		enr = NewEnricher(mem, res, 5, 0.9)
		exp = llmparser.Expense{
			Type:        llmparser.TransactionExpense,
			Amount:      9.5,
			Account:     "1234",
			Description: "Starbucks Coffee",
			Date:        "2024-01-01",
		}
		mem.accounts["1234|"] = "wallet-main"
	})

	It("asks for clarification when parsing failed", func() {
		exp.ParseFailed = true
		exp.FailureReason = "blurry image"

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue())
		Expect(result.Question).To(ContainSubstring("blurry image"))
		Expect(res.calls).To(Equal(0))
	})

	It("asks which wallet an unknown account belongs to, without calling the LLM", func() {
		exp.Account = "9999"

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue())
		Expect(result.Question).To(ContainSubstring("9999"))
		Expect(res.calls).To(Equal(0))
	})

	It("skips the LLM entirely on a high-confidence memory match", func() {
		mem.pairs = append(mem.pairs, memory.TrainingPair{
			RawDesc: "Starbucks Coffee", Name: "Starbucks", CategoryFK: "cat-food", WalletFK: "wallet-main",
		})

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense).NotTo(BeNil())
		Expect(result.Expense.CategoryFK).To(Equal("cat-food"))
		Expect(result.Expense.WalletFK).To(Equal("wallet-main"))
		Expect(result.Expense.Amount).To(Equal(9.5))
		Expect(res.calls).To(Equal(0))
	})

	It("calls the LLM with only the top-N candidates when there is no high-confidence match", func() {
		mem.pairs = append(mem.pairs, memory.TrainingPair{
			RawDesc: "Unrelated Merchant", Name: "Unrelated", CategoryFK: "cat-x", WalletFK: "wallet-main",
		})
		res.resolution = llmparser.Resolution{
			Matched: true, MatchedIndex: -1,
			Name: "Starbucks", CategoryFK: "cat-food", WalletFK: "wallet-main",
		}

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(res.calls).To(Equal(1))
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense.CategoryFK).To(Equal("cat-food"))
		Expect(mem.saveCalls).To(Equal(1)) // saved once after LLM resolution (seed pair was injected directly)
	})

	It("uses the matched candidate's fields when the LLM points at one", func() {
		mem.pairs = append(mem.pairs, memory.TrainingPair{
			RawDesc: "Star Bucks", Name: "Starbucks", CategoryFK: "cat-food", SubCategoryFK: "sub-coffee", WalletFK: "wallet-main",
		})
		res.resolution = llmparser.Resolution{Matched: true, MatchedIndex: 0}

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.CategoryFK).To(Equal("cat-food"))
		Expect(result.Expense.SubCategoryFK).To(Equal("sub-coffee"))
	})

	It("asks the user when the LLM cannot confidently resolve the description", func() {
		mem.pairs = append(mem.pairs, memory.TrainingPair{
			RawDesc: "Unrelated Merchant", Name: "Unrelated", CategoryFK: "cat-x", WalletFK: "wallet-main",
		})
		res.resolution = llmparser.Resolution{NeedsUserInput: true, Question: "What category is this?"}

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue())
		// The question is always built from the raw SMS description, not the LLM's freeform text.
		Expect(result.Question).To(ContainSubstring(`"Starbucks Coffee"`))
		Expect(mem.saveCalls).To(Equal(0)) // nothing saved for this expense
	})
})

var _ = Describe("Enricher.ResolveWithUserInput", func() {
	It("saves the user-supplied mapping to memory and returns the enriched expense", func() {
		ctx := context.Background()
		mem := newFakeMemory()
		enr := NewEnricher(mem, &fakeResolver{}, 5, 0.9)
		exp := llmparser.Expense{Amount: 42, Account: "5555", Description: "New Shop"}

		enriched, err := enr.ResolveWithUserInput(ctx, exp, "New Shop", "cat-new", "", "wallet-savings", false)

		Expect(err).NotTo(HaveOccurred())
		Expect(enriched.CategoryFK).To(Equal("cat-new"))
		Expect(enriched.WalletFK).To(Equal("wallet-savings"))
		Expect(mem.saveCalls).To(Equal(1))
		Expect(mem.saveAccounts).To(Equal(1))

		wallet, ok, err := mem.ResolveAccount(ctx, "5555", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(wallet).To(Equal("wallet-savings"))
	})

	It("returns an error when category or wallet is missing", func() {
		enr := NewEnricher(newFakeMemory(), &fakeResolver{}, 5, 0.9)

		_, err := enr.ResolveWithUserInput(context.Background(), llmparser.Expense{}, "n", "", "", "wallet-x", false)

		Expect(err).To(HaveOccurred())
	})
})
