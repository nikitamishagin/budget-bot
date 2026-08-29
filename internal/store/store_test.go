package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	_ "modernc.org/sqlite"

	"github.com/nikitamishagin/budget-bot/internal/handler"
)

func TestStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Store Suite")
}

const testSchema = `
CREATE TABLE wallets ("wallet_pk" TEXT NOT NULL, "name" TEXT NOT NULL, PRIMARY KEY ("wallet_pk"));
CREATE TABLE categories ("category_pk" TEXT NOT NULL, "name" TEXT NOT NULL, PRIMARY KEY ("category_pk"));
CREATE TABLE transactions (
	"transaction_pk" TEXT NOT NULL,
	"paired_transaction_fk" TEXT NULL,
	"name" TEXT NOT NULL,
	"amount" REAL NOT NULL,
	"note" TEXT NOT NULL,
	"category_fk" TEXT NOT NULL,
	"sub_category_fk" TEXT NULL,
	"wallet_fk" TEXT NOT NULL,
	"date_created" INTEGER NOT NULL,
	"date_time_modified" INTEGER NULL,
	"original_date_due" INTEGER NULL,
	"income" INTEGER NOT NULL DEFAULT 0,
	"period_length" INTEGER NULL,
	"reoccurrence" INTEGER NULL,
	"end_date" INTEGER NULL,
	"upcoming_transaction_notification" INTEGER NULL,
	"type" INTEGER NULL,
	"paid" INTEGER NOT NULL DEFAULT 0,
	"created_another_future_transaction" INTEGER NULL,
	"skip_paid" INTEGER NOT NULL DEFAULT 0,
	"objective_fk" TEXT NULL,
	"objective_loan_fk" TEXT NULL,
	PRIMARY KEY ("transaction_pk")
);
`

// strP and int64P build the pointer values handler.EnrichedExpense's fields need, keeping test
// literals concise.
func strP(s string) *string { return &s }
func int64P(v int64) *int64 { return &v }

// newTestDB creates a temporary Cashew-shaped SQLite file, seeded with rows, and returns an open
// Store plus the file path.
func newTestDB(seedRows ...string) (*Store, string) {
	dir, err := os.MkdirTemp("", "store-test-*")
	Expect(err).NotTo(HaveOccurred())
	path := filepath.Join(dir, "test.sqlite")

	raw, err := sql.Open("sqlite", path)
	Expect(err).NotTo(HaveOccurred())
	_, err = raw.Exec(testSchema)
	Expect(err).NotTo(HaveOccurred())
	for _, row := range seedRows {
		_, err := raw.Exec(row)
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(raw.Close()).To(Succeed())

	s, err := Open(path)
	Expect(err).NotTo(HaveOccurred())
	return s, path
}

var _ = Describe("Open", func() {
	It("opens a valid Cashew-shaped database", func() {
		s, _ := newTestDB()
		defer s.Close()
	})

	It("rejects a file that is not a Cashew database", func() {
		dir, err := os.MkdirTemp("", "store-test-*")
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(dir, "not-a-db.sqlite")
		Expect(os.WriteFile(path, []byte("hello"), 0o644)).To(Succeed())

		_, err = Open(path)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("FindExactMatch and FindCandidates", func() {
	It("finds the most common enrichment for an exact name match", func() {
		s, _ := newTestDB(
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t1', 'Starbucks', 5.0, '', 'cat-coffee', 'wallet-1', 1000, 0, 1, 0)`,
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t2', 'Starbucks', 6.0, '', 'cat-coffee', 'wallet-1', 2000, 0, 1, 0)`,
		)
		defer s.Close()

		c, ok, err := s.FindExactMatch(context.Background(), "Starbucks")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(c.CategoryFK).To(Equal("cat-coffee"))
		Expect(c.WalletFK).To(Equal("wallet-1"))
		Expect(c.Count).To(Equal(2))
	})

	It("returns no match when nothing matches exactly", func() {
		s, _ := newTestDB()
		defer s.Close()

		_, ok, err := s.FindExactMatch(context.Background(), "nowhere")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("finds fuzzy/substring candidates, most frequent first", func() {
		s, _ := newTestDB(
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t1', 'Coffee Shop A', 5.0, '', 'cat-coffee', 'wallet-1', 1000, 0, 1, 0)`,
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t2', 'Coffee Shop B', 6.0, '', 'cat-coffee', 'wallet-1', 2000, 0, 1, 0)`,
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t3', 'Coffee Shop B', 6.0, '', 'cat-coffee', 'wallet-1', 3000, 0, 1, 0)`,
		)
		defer s.Close()

		cands, err := s.FindCandidates(context.Background(), "coffee", 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(cands).To(HaveLen(2))
		Expect(cands[0].Name).To(Equal("Coffee Shop B"))
		Expect(cands[0].Count).To(Equal(2))
	})

	It("never returns more than the requested limit", func() {
		s, _ := newTestDB(
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t1', 'Shop A', 5.0, '', 'cat-1', 'wallet-1', 1000, 0, 1, 0)`,
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, paid, skip_paid)
			 VALUES ('t2', 'Shop B', 6.0, '', 'cat-2', 'wallet-1', 2000, 0, 1, 0)`,
		)
		defer s.Close()

		cands, err := s.FindCandidates(context.Background(), "shop", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(cands).To(HaveLen(1))
	})
})

// dateCreated is a fixed reference timestamp ("2024-01-02T10:00:00Z") used across Insert tests,
// standing in for the value internal/handler.Enricher would have already computed from the raw
// SMS date before building the EnrichedExpense passed to Insert.
var dateCreated = mustParseRFC3339("2024-01-02T10:00:00Z")

func mustParseRFC3339(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

var _ = Describe("FindSimilarTransaction", func() {
	It("copies the no-source fields from the most recent matching transaction", func() {
		s, _ := newTestDB(
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, period_length, reoccurrence, upcoming_transaction_notification, paid, created_another_future_transaction, skip_paid, original_date_due)
			 VALUES ('t1', 'Maxi', 5.0, 'MAXI 1', 'cat-1', 'wallet-1', 1000, 0, 1, 3, 1, 1, 0, 1, 500)`,
			`INSERT INTO transactions (transaction_pk, name, amount, note, category_fk, wallet_fk, date_created, income, period_length, reoccurrence, upcoming_transaction_notification, paid, created_another_future_transaction, skip_paid, original_date_due)
			 VALUES ('t2', 'Maxi', 6.0, 'MAXI 2', 'cat-1', 'wallet-1', 2000, 0, 1, 3, 1, 1, 0, 1, 700)`,
		)
		defer s.Close()

		similar, ok, err := s.FindSimilarTransaction(context.Background(), "MAXI 2")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(similar.OriginalDateDue).To(HaveValue(Equal(int64(700))))
		Expect(similar.PeriodLength).To(HaveValue(Equal(int64(1))))
		Expect(similar.Reoccurrence).To(HaveValue(Equal(int64(3))))
	})

	It("returns ok=false when nothing matches", func() {
		s, _ := newTestDB()
		defer s.Close()

		_, ok, err := s.FindSimilarTransaction(context.Background(), "nowhere")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("Insert", func() {
	It("inserts a new expense", func() {
		s, _ := newTestDB()
		defer s.Close()

		result, err := s.Insert(context.Background(), handler.EnrichedExpense{
			Name:        strP("Starbucks"),
			Amount:      -5.5,
			CategoryFK:  strP("cat-coffee"),
			WalletFK:    strP("wallet-1"),
			DateCreated: dateCreated,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Duplicate).To(BeFalse())
		Expect(result.TransactionPK).NotTo(BeEmpty())

		cands, err := s.FindCandidates(context.Background(), "Starbucks", 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(cands).To(HaveLen(1))
	})

	It("detects a duplicate (same wallet, amount, name and day) and does not insert twice", func() {
		s, _ := newTestDB()
		defer s.Close()

		exp := handler.EnrichedExpense{
			Name:        strP("Starbucks"),
			Amount:      -5.5,
			CategoryFK:  strP("cat-coffee"),
			WalletFK:    strP("wallet-1"),
			DateCreated: dateCreated,
		}

		first, err := s.Insert(context.Background(), exp)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Duplicate).To(BeFalse())

		second, err := s.Insert(context.Background(), exp)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Duplicate).To(BeTrue())
		Expect(second.TransactionPK).To(Equal(first.TransactionPK))

		cands, err := s.FindCandidates(context.Background(), "Starbucks", 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(cands).To(HaveLen(1))
		Expect(cands[0].Count).To(Equal(1))
	})

	It("stores the amount and timestamps exactly as given (Enricher already applied Cashew's sign convention)", func() {
		s, _ := newTestDB()
		defer s.Close()

		result, err := s.Insert(context.Background(), handler.EnrichedExpense{
			Name:        strP("Starbucks"),
			Amount:      -5.5,
			CategoryFK:  strP("cat-coffee"),
			WalletFK:    strP("wallet-1"),
			DateCreated: dateCreated,
			Income:      int64P(0),
		})
		Expect(err).NotTo(HaveOccurred())

		var amount float64
		var gotDateCreated int64
		row := s.db.QueryRow(`SELECT amount, date_created FROM transactions WHERE transaction_pk = ?`, result.TransactionPK)
		Expect(row.Scan(&amount, &gotDateCreated)).To(Succeed())

		// Expenses (income=false) must be stored as a negative amount.
		Expect(amount).To(Equal(-5.5))
		// date_created must be stored as the Unix-seconds epoch (10 digits, not milliseconds)
		// Enrich already computed it as.
		Expect(gotDateCreated).To(Equal(dateCreated))

		incomeResult, err := s.Insert(context.Background(), handler.EnrichedExpense{
			Name:        strP("Salary"),
			Amount:      100,
			CategoryFK:  strP("cat-income"),
			WalletFK:    strP("wallet-1"),
			DateCreated: dateCreated,
			Income:      int64P(1),
		})
		Expect(err).NotTo(HaveOccurred())
		var incomeAmount float64
		Expect(s.db.QueryRow(`SELECT amount FROM transactions WHERE transaction_pk = ?`, incomeResult.TransactionPK).Scan(&incomeAmount)).To(Succeed())
		Expect(incomeAmount).To(Equal(100.0))
	})

	It("does not treat a different day as a duplicate", func() {
		s, _ := newTestDB()
		defer s.Close()

		exp := handler.EnrichedExpense{
			Name:        strP("Starbucks"),
			Amount:      -5.5,
			CategoryFK:  strP("cat-coffee"),
			WalletFK:    strP("wallet-1"),
			DateCreated: dateCreated,
		}
		_, err := s.Insert(context.Background(), exp)
		Expect(err).NotTo(HaveOccurred())

		exp.DateCreated = mustParseRFC3339("2024-01-03T10:00:00Z")
		result, err := s.Insert(context.Background(), exp)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Duplicate).To(BeFalse())
	})
})
