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
	findCalls    int
	topCalls     int
	getCalls     int
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{wallets: map[string]memory.WalletMappings{}}
}

func (f *fakeMemory) FindBestMatch(_ context.Context, pattern string) (memory.Candidate, bool, error) {
	f.findCalls++
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

func (f *fakeMemory) GetTransactionMapping(_ context.Context, pattern string) (memory.TransactionMappings, bool, error) {
	f.getCalls++
	for _, m := range f.mappings {
		if m.TransactionPattern == pattern {
			return m, true, nil
		}
	}
	return memory.TransactionMappings{}, false, nil
}

func (f *fakeMemory) TopCandidates(_ context.Context, _ string, n int) ([]memory.Candidate, error) {
	f.topCalls++
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
	for i, m := range f.mappings {
		if m.TransactionPattern == mapping.TransactionPattern {
			f.mappings[i] = mapping
			return nil
		}
	}
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

// fakeDB is a configurable DBLookup for tests.
type fakeDB struct {
	categories map[string]string
	similar    *DBSimilarFields
	similarErr error
	catErr     error
}

func (f *fakeDB) FindCandidates(context.Context, string, int) ([]DBCandidate, error) {
	return nil, nil
}

func (f *fakeDB) FindSimilar(context.Context, string) (DBSimilarFields, bool, error) {
	if f.similarErr != nil {
		return DBSimilarFields{}, false, f.similarErr
	}
	if f.similar == nil {
		return DBSimilarFields{}, false, nil
	}
	return *f.similar, true, nil
}

func (f *fakeDB) ResolveCategory(_ context.Context, input string) (string, bool, bool, error) {
	if f.catErr != nil {
		return "", false, false, f.catErr
	}
	pk, ok := f.categories[input]
	return pk, false, ok, nil
}

// fakeResolver is a scripted Resolver for tests.
type fakeResolver struct {
	resolution llmparser.Resolution
	err        error
	calls      int
	lastCands  []llmparser.Candidate
}

func (f *fakeResolver) ResolveDescription(_ context.Context, _ llmparser.Expense, candidates []llmparser.Candidate) (llmparser.Resolution, error) {
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
		Expect(result.Kind).To(Equal(QuestionClarify))
		Expect(result.Question).To(ContainSubstring("blurry image"))
		Expect(res.calls).To(Equal(0))
	})

	It("asks which wallet an unknown account belongs to, without calling the LLM", func() {
		exp.Account = "9999"

		result, err := enr.Enrich(ctx, exp)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue())
		Expect(result.Kind).To(Equal(QuestionWallet))
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
		Expect(result.Kind).To(Equal(QuestionName))
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

var _ = Describe("Enricher refunds", func() {
	var (
		ctx context.Context
		mem *fakeMemory
		res *fakeResolver
		enr *Enricher
		db  *fakeDB
		exp llmparser.Expense
	)

	BeforeEach(func() {
		ctx = context.Background()
		mem = newFakeMemory()
		res = &fakeResolver{}
		enr = NewEnricher(mem, res, 5, 0.9)
		db = &fakeDB{categories: map[string]string{"Income": "cat-income"}}
		exp = llmparser.Expense{
			Type: llmparser.TransactionRefund, Amount: 1012, Account: "1234", Description: "Temu.com",
		}
		mem.wallets["1234|"] = memory.WalletMappings{AccountPattern: "1234", WalletFK: "wallet-main"}
	})

	refundRow := func() memory.TransactionMappings {
		var rows []memory.TransactionMappings
		for _, m := range mem.mappings {
			if m.TransactionPattern == "Refund" {
				rows = append(rows, m)
			}
		}
		Expect(rows).To(HaveLen(1))
		return rows[0]
	}

	It("builds a Refund income expense without the LLM and saves one Refund row", func() {
		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense.Name).To(HaveValue(Equal("Refund")))
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal("cat-income")))
		Expect(result.Expense.SubCategoryFK).To(BeNil())
		Expect(result.Expense.Amount).To(Equal(1012.0))
		Expect(result.Expense.Income).To(HaveValue(Equal(int64(1))))
		Expect(result.Expense.Note).To(HaveValue(Equal("Temu.com")))
		Expect(result.Expense.WalletFK).To(HaveValue(Equal("wallet-main")))
		Expect(res.calls).To(Equal(0))
		Expect(mem.findCalls).To(Equal(0))
		Expect(mem.topCalls).To(Equal(0))
		Expect(mem.getCalls).To(Equal(1))
		Expect(mem.saveCalls).To(Equal(1))
		row := refundRow()
		Expect(row.Name).To(HaveValue(Equal("Refund")))
		Expect(row.CategoryFK).To(HaveValue(Equal("cat-income")))
		Expect(row.Income).To(HaveValue(Equal(int64(1))))
		Expect(row.Note).To(BeNil())
	})

	It("never uses the merchant description as a pattern and leaves merchant mappings untouched", func() {
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Temu.com", Name: strPtr("Temu"), CategoryFK: strPtr("cat-shopping"),
		})

		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Name).To(HaveValue(Equal("Refund")))
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal("cat-income")))
		Expect(mem.mappings).To(HaveLen(2))
		Expect(mem.mappings[0].TransactionPattern).To(Equal("Temu.com"))
		Expect(mem.mappings[0].Name).To(HaveValue(Equal("Temu")))
		Expect(mem.mappings[0].CategoryFK).To(HaveValue(Equal("cat-shopping")))
		Expect(mem.saveCalls).To(Equal(1))
		Expect(refundRow().TransactionPattern).To(Equal("Refund"))
	})

	It("asks for the wallet when the account is unknown", func() {
		exp.Account = "9999"

		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue())
		Expect(result.Kind).To(Equal(QuestionWallet))
	})

	It("returns an error when the Income category is missing or no database is given", func() {
		db.categories = map[string]string{}
		_, err := enr.Enrich(ctx, exp, db)
		Expect(err).To(MatchError(ContainSubstring("Income category not found")))

		_, err = enr.Enrich(ctx, exp)
		Expect(err).To(HaveOccurred())
	})

	It("on a memory hit, cached fields win over SQLite while OriginalDateDue comes from SQLite", func() {
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Refund", Name: strPtr("Refund"), CategoryFK: strPtr("cat-income"),
			Paid: int64Ptr(1), Type: int64Ptr(7),
		})
		due := int64(555)
		db.similar = &DBSimilarFields{Paid: int64Ptr(0), Type: int64Ptr(9), OriginalDateDue: &due}

		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Paid).To(HaveValue(Equal(int64(1))))
		Expect(result.Expense.Type).To(HaveValue(Equal(int64(7))))
		Expect(result.Expense.OriginalDateDue).To(HaveValue(Equal(int64(555))))
		Expect(mem.saveCalls).To(Equal(1))
		Expect(mem.mappings).To(HaveLen(1))
	})

	It("on a memory hit, tops up nil fields from SQLite", func() {
		mem.mappings = append(mem.mappings, memory.TransactionMappings{
			TransactionPattern: "Refund", Paid: int64Ptr(1),
		})
		db.similar = &DBSimilarFields{Paid: int64Ptr(0), Type: int64Ptr(9), SkipPaid: int64Ptr(0)}

		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Paid).To(HaveValue(Equal(int64(1))))
		Expect(result.Expense.Type).To(HaveValue(Equal(int64(9))))
		Expect(result.Expense.SkipPaid).To(HaveValue(Equal(int64(0))))
		row := refundRow()
		Expect(row.Paid).To(HaveValue(Equal(int64(1))))
		Expect(row.Type).To(HaveValue(Equal(int64(9))))
	})

	It("on a memory miss, fills from SQLite and saves the Refund row", func() {
		due := int64(777)
		db.similar = &DBSimilarFields{Paid: int64Ptr(1), Type: int64Ptr(7), OriginalDateDue: &due}

		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Paid).To(HaveValue(Equal(int64(1))))
		Expect(result.Expense.Type).To(HaveValue(Equal(int64(7))))
		Expect(result.Expense.OriginalDateDue).To(HaveValue(Equal(int64(777))))
		Expect(mem.saveCalls).To(Equal(1))
		row := refundRow()
		Expect(row.Paid).To(HaveValue(Equal(int64(1))))
		Expect(row.Type).To(HaveValue(Equal(int64(7))))
	})

	It("leaves no-source fields NULL when there is no similar transaction", func() {
		db.similar = nil

		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Paid).To(BeNil())
		Expect(result.Expense.Type).To(BeNil())
		Expect(result.Expense.OriginalDateDue).To(BeNil())
		Expect(refundRow().Paid).To(BeNil())
	})

	It("reuses the single Refund row for a refund from a different merchant", func() {
		db.similar = &DBSimilarFields{Paid: int64Ptr(1)}
		_, err := enr.Enrich(ctx, exp, db)
		Expect(err).NotTo(HaveOccurred())

		exp.Description = "Amazon"
		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Note).To(HaveValue(Equal("Amazon")))
		Expect(result.Expense.Paid).To(HaveValue(Equal(int64(1))))
		Expect(mem.mappings).To(HaveLen(1))
		Expect(refundRow().TransactionPattern).To(Equal("Refund"))
	})

	It("never wipes cached values when SQLite no longer has a similar transaction", func() {
		db.similar = &DBSimilarFields{Paid: int64Ptr(1), Type: int64Ptr(7)}
		_, err := enr.Enrich(ctx, exp, db)
		Expect(err).NotTo(HaveOccurred())

		db.similar = nil
		result, err := enr.Enrich(ctx, exp, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.Expense.Paid).To(HaveValue(Equal(int64(1))))
		Expect(result.Expense.Type).To(HaveValue(Equal(int64(7))))
		row := refundRow()
		Expect(row.Paid).To(HaveValue(Equal(int64(1))))
		Expect(row.Type).To(HaveValue(Equal(int64(7))))
	})

	It("saves the Refund row from ResolveWithUserInput, ignoring the user's name and category", func() {
		enriched, err := enr.ResolveWithUserInput(ctx, exp, "Temu", "cat-shopping", "", "wallet-main", false, db)

		Expect(err).NotTo(HaveOccurred())
		Expect(enriched.Name).To(HaveValue(Equal("Refund")))
		Expect(enriched.CategoryFK).To(HaveValue(Equal("cat-income")))
		Expect(mem.saveCalls).To(Equal(1))
		Expect(refundRow().TransactionPattern).To(Equal("Refund"))
	})
})

func int64Ptr(v int64) *int64 { return &v }
