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
	"paid" INTEGER NOT NULL DEFAULT 0,
	"skip_paid" INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY ("transaction_pk")
);
`

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

var _ = Describe("Insert", func() {
	It("inserts a new expense", func() {
		s, _ := newTestDB()
		defer s.Close()

		result, err := s.Insert(context.Background(), handler.EnrichedExpense{
			Name:       "Starbucks",
			Amount:     5.5,
			CategoryFK: "cat-coffee",
			WalletFK:   "wallet-1",
			Date:       "2024-01-02T10:00:00Z",
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
			Name:       "Starbucks",
			Amount:     5.5,
			CategoryFK: "cat-coffee",
			WalletFK:   "wallet-1",
			Date:       "2024-01-02T10:00:00Z",
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

	It("stores amount with Cashew's sign convention and timestamps in Unix seconds", func() {
		s, _ := newTestDB()
		defer s.Close()

		before := time.Now().Unix()
		result, err := s.Insert(context.Background(), handler.EnrichedExpense{
			Name:       "Starbucks",
			Amount:     5.5,
			CategoryFK: "cat-coffee",
			WalletFK:   "wallet-1",
			Date:       "2024-01-02T10:00:00Z",
			Income:     false,
		})
		Expect(err).NotTo(HaveOccurred())

		var amount float64
		var dateCreated int64
		row := s.db.QueryRow(`SELECT amount, date_created FROM transactions WHERE transaction_pk = ?`, result.TransactionPK)
		Expect(row.Scan(&amount, &dateCreated)).To(Succeed())

		// Expenses (income=false) must be stored as a negative amount, per llama-issue.md "Data
		// format in the sqlite database".
		Expect(amount).To(Equal(-5.5))
		// date_created must be a Unix-seconds epoch (10 digits, not milliseconds), matching the
		// value implied by the RFC3339 exp.Date.
		expected, _ := time.Parse(time.RFC3339, "2024-01-02T10:00:00Z")
		Expect(dateCreated).To(Equal(expected.Unix()))
		Expect(dateCreated).To(BeNumerically("<", before+86400)) // sane seconds-range, not millis

		incomeResult, err := s.Insert(context.Background(), handler.EnrichedExpense{
			Name:       "Salary",
			Amount:     100,
			CategoryFK: "cat-income",
			WalletFK:   "wallet-1",
			Date:       "2024-01-02T10:00:00Z",
			Income:     true,
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
			Name:       "Starbucks",
			Amount:     5.5,
			CategoryFK: "cat-coffee",
			WalletFK:   "wallet-1",
			Date:       "2024-01-02T10:00:00Z",
		}
		_, err := s.Insert(context.Background(), exp)
		Expect(err).NotTo(HaveOccurred())

		exp.Date = "2024-01-03T10:00:00Z"
		result, err := s.Insert(context.Background(), exp)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Duplicate).To(BeFalse())
	})
})
