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
		_, err = store.pool.Exec(ctx, "TRUNCATE training_pairs, bank_account_mappings")
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("SaveTrainingPair and FindBestMatch", func() {
		It("returns no match when memory is empty", func() {
			_, ok, err := store.FindBestMatch(ctx, "Starbucks")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("finds an exact topic match with score 1", func() {
			Expect(store.SaveTrainingPair(ctx, TrainingPair{
				RawTopic:   "Starbucks Coffee",
				Name:       "Starbucks",
				CategoryFK: "cat-food",
				WalletFK:   "wallet-main",
			})).To(Succeed())

			candidate, ok, err := store.FindBestMatch(ctx, "Starbucks Coffee")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(candidate.CategoryFK).To(Equal("cat-food"))
			Expect(candidate.WalletFK).To(Equal("wallet-main"))
			Expect(candidate.Score).To(BeNumerically(">=", HighConfidenceThreshold))
		})

		It("increments hit_count when saving the same pair again", func() {
			pair := TrainingPair{
				RawTopic:   "Uber Trip",
				Name:       "Uber",
				CategoryFK: "cat-transport",
				WalletFK:   "wallet-main",
			}
			Expect(store.SaveTrainingPair(ctx, pair)).To(Succeed())
			Expect(store.SaveTrainingPair(ctx, pair)).To(Succeed())

			var hitCount int
			row := store.pool.QueryRow(ctx, "SELECT hit_count FROM training_pairs WHERE raw_topic = $1", "Uber Trip")
			Expect(row.Scan(&hitCount)).To(Succeed())
			Expect(hitCount).To(Equal(2))
		})

		It("rejects a pair with an empty raw topic", func() {
			err := store.SaveTrainingPair(ctx, TrainingPair{CategoryFK: "c", WalletFK: "w"})

			Expect(err).To(HaveOccurred())
		})
	})

	Describe("TopCandidates", func() {
		BeforeEach(func() {
			Expect(store.SaveTrainingPair(ctx, TrainingPair{
				RawTopic: "Amazon Marketplace", Name: "Amazon", CategoryFK: "cat-shopping", WalletFK: "wallet-main",
			})).To(Succeed())
			Expect(store.SaveTrainingPair(ctx, TrainingPair{
				RawTopic: "Amazon Prime Video", Name: "Amazon Prime", CategoryFK: "cat-subscriptions", WalletFK: "wallet-main",
			})).To(Succeed())
			Expect(store.SaveTrainingPair(ctx, TrainingPair{
				RawTopic: "Local Grocery Store", Name: "Grocery", CategoryFK: "cat-food", WalletFK: "wallet-main",
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

		It("does not dump unrelated rows for a completely different topic", func() {
			candidates, err := store.TopCandidates(ctx, "zzz-completely-unrelated-xyz", 5)

			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(BeEmpty())
		})
	})

	Describe("SaveAccountMapping and ResolveAccount", func() {
		It("returns not found for an unknown account", func() {
			_, ok, err := store.ResolveAccount(ctx, "**** 1234", "USD")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("saves and resolves an account mapping", func() {
			Expect(store.SaveAccountMapping(ctx, "**** 1234", "USD", "wallet-checking")).To(Succeed())

			walletFK, ok, err := store.ResolveAccount(ctx, "**** 1234", "USD")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(walletFK).To(Equal("wallet-checking"))
		})

		It("updates the wallet when the mapping already exists", func() {
			Expect(store.SaveAccountMapping(ctx, "**** 1234", "USD", "wallet-checking")).To(Succeed())
			Expect(store.SaveAccountMapping(ctx, "**** 1234", "USD", "wallet-savings")).To(Succeed())

			walletFK, ok, err := store.ResolveAccount(ctx, "**** 1234", "USD")

			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(walletFK).To(Equal("wallet-savings"))
		})

		It("maps the same account to different wallets depending on currency", func() {
			Expect(store.SaveAccountMapping(ctx, "TBC ****", "RSD", "wallet-rsd")).To(Succeed())
			Expect(store.SaveAccountMapping(ctx, "TBC ****", "USD", "wallet-usd")).To(Succeed())

			rsdWallet, ok, err := store.ResolveAccount(ctx, "TBC ****", "RSD")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(rsdWallet).To(Equal("wallet-rsd"))

			usdWallet, ok, err := store.ResolveAccount(ctx, "TBC ****", "USD")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(usdWallet).To(Equal("wallet-usd"))
		})
	})
})
