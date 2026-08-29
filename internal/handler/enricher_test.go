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
	mappings     []memory.TransactionMappings
	wallets      map[string]memory.WalletMappings
	saveCalls    int
	saveAccounts int
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{wallets: map[string]memory.WalletMappings{}}
}

func (f *fakeMemory) FindBestMatch(_ context.Context, pattern string) (memory.Candidate, bool, error) {
	for _, m := range f.mappings {
		if m.TransactionPattern == pattern {
			return memory.Candidate{TransactionMappings: m, Score: 1.0}, true, nil
		}
	}
	if len(f.mappings) == 0 {
		return memory.Candidate{}, false, nil
	}
	return memory.Candidate{TransactionMappings: f.mappings[0], Score: 0.3}, true, nil
}

func (f *fakeMemory) TopCandidates(_ context.Context, _ string, n int) ([]memory.Candidate, error) {
	var out []memory.Candidate
	for i, m := range f.mappings {
		if i >= n {
			break
		}
		out = append(out, memory.Candidate{TransactionMappings: m, Score: 0.5})
	}
	return out, nil
}

func (f *fakeMemory) SaveTransactionMapping(_ context.Context, mapping memory.TransactionMappings) error {
	f.saveCalls++
	f.mappings = append(f.mappings, mapping)
	return nil
}

func (f *fakeMemory) ResolveWallet(_ context.Context, accountPattern, currency string) (memory.WalletMappings, bool, error) {
	wallet, ok := f.wallets[accountPattern+"|"+currency]
	return wallet, ok, nil
}

func (f *fakeMemory) SaveWalletMapping(_ context.Context, mapping memory.WalletMappings) error {
	f.saveAccounts++
	f.wallets[mapping.AccountPattern+"|"+mapping.Currency] = mapping
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
		mem.wallets["1234|"] = memory.WalletMappings{AccountPattern: "1234", WalletFK: "wallet-main"}
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
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Starbucks Coffee", Name: strPtr("Starbucks"), CategoryFK: strPtr("cat-food"),
		})

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense).NotTo(BeNil())
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal("cat-food")))
		Expect(result.Expense.WalletFK).To(HaveValue(Equal("wallet-main")))
		Expect(result.Expense.Amount).To(Equal(-9.5))
		Expect(res.calls).To(Equal(0))
	})

	It("calls the LLM with only the top-N candidates when there is no high-confidence match", func() {
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Unrelated Merchant", Name: strPtr("Unrelated"), CategoryFK: strPtr("cat-x"),
		})
		res.resolution = llmparser.Resolution{
			Matched: true, MatchedIndex: -1,
			Name: "Starbucks", CategoryFK: "cat-food", WalletFK: "wallet-main",
		}

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(res.calls).To(Equal(1))
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal("cat-food")))
		Expect(mem.saveCalls).To(Equal(1)) // saved once after LLM resolution (seed mapping was injected directly)
	})

	It("uses the matched candidate's fields when the LLM points at one", func() {
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Star Bucks", Name: strPtr("Starbucks"), CategoryFK: strPtr("cat-food"), SubCategoryFK: strPtr("sub-coffee"),
		})
		res.resolution = llmparser.Resolution{Matched: true, MatchedIndex: 0}

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal("cat-food")))
		Expect(result.Expense.SubCategoryFK).To(HaveValue(Equal("sub-coffee")))
	})

	It("asks the user when the LLM cannot confidently resolve the description", func() {
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Unrelated Merchant", Name: strPtr("Unrelated"), CategoryFK: strPtr("cat-x"),
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
		Expect(enriched.CategoryFK).To(HaveValue(Equal("cat-new")))
		Expect(enriched.WalletFK).To(HaveValue(Equal("wallet-savings")))
		Expect(mem.saveCalls).To(Equal(1))
		Expect(mem.saveAccounts).To(Equal(1))

		wallet, ok, err := mem.ResolveWallet(ctx, "5555", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(wallet.WalletFK).To(Equal("wallet-savings"))
	})

	It("returns an error when category or wallet is missing", func() {
		enr := NewEnricher(newFakeMemory(), &fakeResolver{}, 5, 0.9)

		_, err := enr.ResolveWithUserInput(context.Background(), llmparser.Expense{}, "n", "", "", "wallet-x", false)

		Expect(err).To(HaveOccurred())
	})
})
