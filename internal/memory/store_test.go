package memory

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMemory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Memory Suite")
}

// testDSN builds the DSN for the local dev Postgres instance from postgres.env defaults,
// overridable via the MEMORY_TEST_DSN environment variable.
func testDSN() string {
	if dsn := os.Getenv("MEMORY_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://devuser:devpass@127.0.0.1:5432/expense_agent?sslmode=disable"
}

func strPtr(s string) *string {
	return &s
}

func int64Ptr(v int64) *int64 {
	return &v
}

var _ = Describe("Store", func() {
	var (
		ctx   context.Context
		store *Store
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		DeferCleanup(cancel)

		var err error
		store, err = New(ctx, testDSN())
		if err != nil {
			Skip(fmt.Sprintf("postgres not reachable, skipping: %v", err))
		}
		DeferCleanup(store.Close)

		Expect(store.Migrate(ctx)).To(Succeed())
		_, err = store.pool.Exec(ctx, "TRUNCATE transaction_mappings, wallet_mappings")
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("SaveTransactionMapping and FindBestMatch", func() {
		It("returns no match when memory is empty", func() {
			_, ok, err := store.FindBestMatch(ctx, "Starbucks")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("finds an exact pattern match with score 1", func() {
			Expect(store.SaveTransactionMapping(ctx, TransactionMappings{
				TransactionPattern: "Starbucks Coffee",
				Name:               strPtr("Starbucks"),
				CategoryFK:         strPtr("cat-food"),
			})).To(Succeed())

			candidate, ok, err := store.FindBestMatch(ctx, "Starbucks Coffee")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(candidate.CategoryFK).NotTo(BeNil())
			Expect(*candidate.CategoryFK).To(Equal("cat-food"))
			Expect(candidate.Score).To(BeNumerically(">=", HighConfidenceThreshold))
		})

		It("leaves fields with no source in raw SMS data as nil when not provided", func() {
			Expect(store.SaveTransactionMapping(ctx, TransactionMappings{
				TransactionPattern: "Uber Trip",
				Name:               strPtr("Uber"),
				CategoryFK:         strPtr("cat-transport"),
			})).To(Succeed())

			candidate, ok, err := store.FindBestMatch(ctx, "Uber Trip")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(candidate.PairedTransactionFK).To(BeNil())
			Expect(candidate.PeriodLength).To(BeNil())
			Expect(candidate.ObjectiveFK).To(BeNil())
		})

		It("copies fields resolved from a similar sqlite transaction when provided", func() {
			Expect(store.SaveTransactionMapping(ctx, TransactionMappings{
				TransactionPattern: "Rent Payment",
				Name:               strPtr("Rent"),
				CategoryFK:         strPtr("cat-housing"),
				PeriodLength:       int64Ptr(1),
				Reoccurrence:       int64Ptr(2),
				Type:               int64Ptr(3),
			})).To(Succeed())

			candidate, ok, err := store.FindBestMatch(ctx, "Rent Payment")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(candidate.PeriodLength).NotTo(BeNil())
			Expect(*candidate.PeriodLength).To(Equal(int64(1)))
			Expect(*candidate.Reoccurrence).To(Equal(int64(2)))
			Expect(*candidate.Type).To(Equal(int64(3)))
		})

		It("updates an existing mapping instead of creating a duplicate row", func() {
			mapping := TransactionMappings{
				TransactionPattern: "Uber Trip",
				Name:               strPtr("Uber"),
				CategoryFK:         strPtr("cat-transport"),
			}
			Expect(store.SaveTransactionMapping(ctx, mapping)).To(Succeed())
			mapping.CategoryFK = strPtr("cat-travel")
			Expect(store.SaveTransactionMapping(ctx, mapping)).To(Succeed())

			var hitCount int
			row := store.pool.QueryRow(ctx, "SELECT hit_count FROM transaction_mappings WHERE transaction_pattern = $1", "Uber Trip")
			Expect(row.Scan(&hitCount)).To(Succeed())
			Expect(hitCount).To(Equal(2))

			candidate, ok, err := store.FindBestMatch(ctx, "Uber Trip")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(*candidate.CategoryFK).To(Equal("cat-travel"))
		})

		It("rejects a mapping with an empty transaction pattern", func() {
			err := store.SaveTransactionMapping(ctx, TransactionMappings{CategoryFK: strPtr("c")})

			Expect(err).To(HaveOccurred())
		})
	})

	Describe("TopCandidates", func() {
		BeforeEach(func() {
			Expect(store.SaveTransactionMapping(ctx, TransactionMappings{
				TransactionPattern: "Amazon Marketplace", Name: strPtr("Amazon"), CategoryFK: strPtr("cat-shopping"),
			})).To(Succeed())
			Expect(store.SaveTransactionMapping(ctx, TransactionMappings{
				TransactionPattern: "Amazon Prime Video", Name: strPtr("Amazon Prime"), CategoryFK: strPtr("cat-subscriptions"),
			})).To(Succeed())
			Expect(store.SaveTransactionMapping(ctx, TransactionMappings{
				TransactionPattern: "Local Grocery Store", Name: strPtr("Grocery"), CategoryFK: strPtr("cat-food"),
			})).To(Succeed())
		})

		It("returns candidates similar to the query ordered by score", func() {
			candidates, err := store.TopCandidates(ctx, "Amazon", 5)

			Expect(err).NotTo(HaveOccurred())
			Expect(len(candidates)).To(BeNumerically(">=", 1))
			for i := 1; i < len(candidates); i++ {
				Expect(candidates[i-1].Score).To(BeNumerically(">=", candidates[i].Score))
			}
		})

		It("respects the limit", func() {
			candidates, err := store.TopCandidates(ctx, "Amazon", 1)

			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(1))
		})

		It("does not dump unrelated rows for a completely different pattern", func() {
			candidates, err := store.TopCandidates(ctx, "zzz-completely-unrelated-xyz", 5)

			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(BeEmpty())
		})
	})

	Describe("SaveWalletMapping and ResolveWallet", func() {
		It("returns not found for an unknown account", func() {
			_, ok, err := store.ResolveWallet(ctx, "**** 1234", "USD")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("saves and resolves a wallet mapping", func() {
			Expect(store.SaveWalletMapping(ctx, WalletMappings{
				AccountPattern: "**** 1234", Currency: "USD", Name: "Checking", Type: "bank", WalletFK: "wallet-checking",
			})).To(Succeed())

			wallet, ok, err := store.ResolveWallet(ctx, "**** 1234", "USD")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(wallet.WalletFK).To(Equal("wallet-checking"))
		})

		It("updates the wallet when the mapping already exists", func() {
			Expect(store.SaveWalletMapping(ctx, WalletMappings{
				AccountPattern: "**** 1234", Currency: "USD", Name: "Checking", Type: "bank", WalletFK: "wallet-checking",
			})).To(Succeed())
			Expect(store.SaveWalletMapping(ctx, WalletMappings{
				AccountPattern: "**** 1234", Currency: "USD", Name: "Savings", Type: "bank", WalletFK: "wallet-savings",
			})).To(Succeed())

			wallet, ok, err := store.ResolveWallet(ctx, "**** 1234", "USD")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(wallet.WalletFK).To(Equal("wallet-savings"))
		})

		It("maps the same account to different wallets depending on currency", func() {
			Expect(store.SaveWalletMapping(ctx, WalletMappings{
				AccountPattern: "TBC ****", Currency: "RSD", Name: "TBC RSD", Type: "bank", WalletFK: "wallet-rsd",
			})).To(Succeed())
			Expect(store.SaveWalletMapping(ctx, WalletMappings{
				AccountPattern: "TBC ****", Currency: "USD", Name: "TBC USD", Type: "bank", WalletFK: "wallet-usd",
			})).To(Succeed())

			rsdWallet, ok, err := store.ResolveWallet(ctx, "TBC ****", "RSD")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(rsdWallet.WalletFK).To(Equal("wallet-rsd"))

			usdWallet, ok, err := store.ResolveWallet(ctx, "TBC ****", "USD")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(usdWallet.WalletFK).To(Equal("wallet-usd"))
		})

		It("rejects a mapping with an empty account pattern", func() {
			err := store.SaveWalletMapping(ctx, WalletMappings{WalletFK: "wallet-x"})

			Expect(err).To(HaveOccurred())
		})

		It("rejects a mapping with an empty wallet_fk", func() {
			err := store.SaveWalletMapping(ctx, WalletMappings{AccountPattern: "1234"})

			Expect(err).To(HaveOccurred())
		})
	})
})
