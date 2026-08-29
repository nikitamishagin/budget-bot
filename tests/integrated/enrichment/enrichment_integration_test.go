//go:build integration

// Package enrichment_test contains integration tests for the raw-SMS-to-enriched-expense
// pipeline (internal/handler.Enricher), wired against real backends: a Postgres agent memory
// (internal/memory) and a Cashew-shaped SQLite file copied from testdata/test-transaction-data.sql
// (a real, minimal Cashew export), plus a real, locally running LLM inference server
// (internal/llmparser.Client) for the cases that require it. They are excluded from the default
// `go test ./...` run via the "integration" build tag since they require external
// infrastructure. Run them explicitly with:
//
//	go test -tags=integration ./tests/integrated/enrichment/...
//
// Both Postgres and LLM connection settings are taken from the standard config file (see
// config.example.yaml), overridable via BUDGET_BOT_TEST_CONFIG; the suite always connects to a
// dedicated test database (see defaultTestMemoryDBName), never the configured production one.
package enrichment_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nikitamishagin/budget-bot/internal/config"
	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/nikitamishagin/budget-bot/internal/store"
)

func TestEnrichmentIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Enrichment Integration Suite")
}

// defaultTestConfigPath points at the repository's example config, used only to reach the
// locally running llama.cpp server; it can be overridden via BUDGET_BOT_TEST_CONFIG.
const defaultTestConfigPath = "../../../config.example.yaml"

// rawDataPath and sqliteFixturePath point at the test fixtures for this suite, kept under
// testdata/ (a Go-conventional directory name that the toolchain never treats as a package)
// rather than the repository's ./data/ directory, which is only a scratch/working directory for
// the running agent, not a place for checked-in test fixtures. sqliteFixturePath is a real,
// minimal Cashew SQLite export (not a CSV/SQL script), so it is copied as-is rather than seeded
// row by row.
const (
	rawDataPath       = "testdata/test-raw-data.json"
	sqliteFixturePath = "testdata/test-transaction-data.sql"
)

// realWalletFK/realCategoryFK/realDrinksCategoryFK are taken directly from
// testdata/test-transaction-data.sql: they are the actual wallet/category the pre-existing
// "Maxi"/"Lidl"/"Drinks"/"Sank" transactions use, matching testdata/answers.json exactly.
// testAccount is the bank_account value shared by every entry in testdata/test-raw-data.json.
const (
	realWalletFK       = "6986664f-0ce4-4bfd-938b-db76cec82754"
	realCategoryFK     = "2"
	realSankCategoryFK = "1"
	testAccount        = "1391"
)

// defaultTestMemoryDBName is a dedicated Postgres database, separate from the memory.dbname
// configured for the running agent, so this suite's TRUNCATE-heavy setup can never touch real
// agent memory. Override via MEMORY_TEST_DBNAME (e.g. to point at a CI-provisioned database).
const defaultTestMemoryDBName = "expense_agent_test"

// testDSN builds the Postgres DSN for this suite: connection details (host, port, user,
// password, sslmode) come from the standard config file (see config.example.yaml, overridable
// via BUDGET_BOT_TEST_CONFIG), so no credentials are ever hardcoded here, but the database name
// is always overridden to a dedicated test database (see defaultTestMemoryDBName) rather than
// the configured production one.
func testDSN() string {
	cfgPath := defaultTestConfigPath
	if p := os.Getenv("BUDGET_BOT_TEST_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	Expect(err).NotTo(HaveOccurred(), "load test config from %q", cfgPath)

	dbName := defaultTestMemoryDBName
	if n := os.Getenv("MEMORY_TEST_DBNAME"); n != "" {
		dbName = n
	}
	cfg.Memory.DBName = dbName

	return cfg.DSN()
}

// loadRawExpenses reads testdata/test-raw-data.json into llmparser.Expense values.
func loadRawExpenses() []llmparser.Expense {
	data, err := os.ReadFile(rawDataPath)
	Expect(err).NotTo(HaveOccurred(), "read %s", rawDataPath)

	var expenses []llmparser.Expense
	Expect(json.Unmarshal(data, &expenses)).To(Succeed(), "unmarshal %s", rawDataPath)
	return expenses
}

// findExpense returns the raw expense whose description exactly matches desc, failing the test
// if testdata/test-raw-data.json no longer contains it.
func findExpense(expenses []llmparser.Expense, desc string) llmparser.Expense {
	for _, e := range expenses {
		if e.Description == desc {
			return e
		}
	}
	Fail(fmt.Sprintf("no raw expense with description %q found in %s", desc, rawDataPath))
	return llmparser.Expense{}
}

// scratchDataDir is the repository's ./data directory: a gitignored scratch area for temporary
// runtime files (the running agent's SQLite files, screenshots, etc.). Any temporary file this
// suite creates (the copied SQLite fixture) belongs there too, not under os.TempDir().
const scratchDataDir = "../../../data"

// newSQLiteFixture copies testdata/test-transaction-data.sql (a real, minimal Cashew export)
// into a temporary file under scratchDataDir and opens it, so every test starts from an
// identical, untouched copy of the fixture regardless of run order.
func newSQLiteFixture() *store.Store {
	dir, err := os.MkdirTemp(scratchDataDir, "enrichment-integration-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "cashew.sqlite")

	src, err := os.Open(sqliteFixturePath)
	Expect(err).NotTo(HaveOccurred(), "open %s", sqliteFixturePath)
	defer src.Close()

	dst, err := os.Create(path)
	Expect(err).NotTo(HaveOccurred())
	_, err = io.Copy(dst, src)
	Expect(err).NotTo(HaveOccurred())
	Expect(dst.Close()).To(Succeed())

	s, err := store.Open(path)
	Expect(err).NotTo(HaveOccurred(), "open copied sqlite fixture")
	return s
}

// newRealMemoryStore connects to the dedicated test Postgres database (see testDSN), migrates
// it and truncates the memory tables so each test starts from a clean slate. It skips the test
// (rather than failing it) when Postgres is not reachable, mirroring
// internal/memory/store_test.go. Truncating is done through a separate pgxpool connection
// (rather than through *memory.Store, which deliberately does not expose its pool) since only
// tests should ever wipe this data.
func newRealMemoryStore(ctx context.Context) *memory.Store {
	mem, err := memory.New(ctx, testDSN())
	if err != nil {
		Skip(fmt.Sprintf("postgres not reachable, skipping: %v", err))
	}
	DeferCleanup(mem.Close)

	Expect(mem.Migrate(ctx)).To(Succeed())
	truncateMemoryTables(ctx)
	return mem
}

// newRawPool opens a plain pgxpool.Pool to the dedicated test Postgres database, for tests that
// need to run raw SQL against transaction_mappings (e.g. to simulate a mapping learned in an
// earlier session) rather than going through *memory.Store's public API.
func newRawPool(ctx context.Context) *pgxpool.Pool {
	pool, err := pgxpool.New(ctx, testDSN())
	Expect(err).NotTo(HaveOccurred())
	return pool
}

// truncateMemoryTables wipes the memory tables so each test starts from a clean slate.
func truncateMemoryTables(ctx context.Context) {
	pool := newRawPool(ctx)
	defer pool.Close()

	_, err := pool.Exec(ctx, "TRUNCATE transaction_mappings, wallet_mappings")
	Expect(err).NotTo(HaveOccurred())
}

// newRealLLMResolver builds an *llmparser.Client from the standard config file, to be used as
// the Enricher's Resolver. It skips the test when the LLM server is not reachable.
func newRealLLMResolver(ctx context.Context) *llmparser.Client {
	cfgPath := defaultTestConfigPath
	if p := os.Getenv("BUDGET_BOT_TEST_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	Expect(err).NotTo(HaveOccurred(), "load test config from %q", cfgPath)

	apiKey, err := cfg.APIKey()
	Expect(err).NotTo(HaveOccurred())

	client := llmparser.NewClient(
		cfg.LLM.BaseURL,
		apiKey,
		cfg.LLM.Model,
		time.Duration(cfg.LLM.TimeoutSeconds)*time.Second,
		cfg.LLM.MaxTokensParse,
		cfg.LLM.MaxTokensResolve,
	)

	// A short resolution call is enough to confirm the server is actually reachable before
	// running a whole spec against it.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := client.ResolveTopic(pingCtx, llmparser.Expense{Description: "ping"}, nil); err != nil {
		Skip(fmt.Sprintf("llm server not reachable, skipping: %v", err))
	}
	return client
}

// unreachableResolver fails the test if the LLM is ever consulted: used to prove a
// high-confidence memory match short-circuits before reaching the LLM.
type unreachableResolver struct{}

func (unreachableResolver) ResolveTopic(context.Context, llmparser.Expense, []llmparser.Candidate) (llmparser.Resolution, error) {
	Fail("the LLM must not be called for a high-confidence memory match")
	return llmparser.Resolution{}, nil
}

var _ = Describe("Enricher.Enrich against real memory + SQLite backends", func() {
	var (
		ctx context.Context
		mem *memory.Store
		db  *store.Store
		raw []llmparser.Expense
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		mem = newRealMemoryStore(ctx)

		db = newSQLiteFixture()
		DeferCleanup(db.Close)

		raw = loadRawExpenses()

		// The wallet mapping is shared by every case below: the raw data always uses the same
		// bank_account ("1391"), and wallet resolution happens before any memory/LLM lookup for
		// the description, so every case needs it resolved to reach that lookup at all.
		Expect(mem.SaveWalletMapping(ctx, memory.WalletMappings{
			AccountPattern: testAccount,
			Currency:       "RSD",
			Name:           "Main card",
			Type:           "bank",
			WalletFK:       realWalletFK,
		})).To(Succeed())
	})

	It("exact match: resolves directly from an exact memory pattern, without calling the LLM", func() {
		exp := findExpense(raw, "KAFETERIJA SANK")

		Expect(mem.SaveTransactionMapping(ctx, memory.TransactionMappings{
			TransactionPattern: exp.Description,
			Name:               ptr("Sank"),
			CategoryFK:         ptr(realSankCategoryFK),
			SubCategoryFK:      ptr("2b2c6cc2-6504-4360-bab5-ecbbd7889263"),
			Income:             int64Ptr(0),
		})).To(Succeed())

		enr := handler.NewEnricher(mem, unreachableResolver{}, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense).NotTo(BeNil())
		Expect(result.Expense.Name).To(HaveValue(Equal("Sank")))
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal(realSankCategoryFK)))
		Expect(result.Expense.WalletFK).To(HaveValue(Equal(realWalletFK)))
		Expect(result.Expense.Amount).To(Equal(-exp.Amount))
		// The no-source fields (see AGENTS.md's field resolution rule) must be copied from the
		// most similar existing "Sank" transaction in testdata/test-transaction-data.sql, not
		// invented, since they have no source in the raw SMS data.
		Expect(result.Expense.PeriodLength).To(HaveValue(BeNumerically("==", 1)))
		Expect(result.Expense.Reoccurrence).To(HaveValue(BeNumerically("==", 3)))
	})

	It("llm found match: a similar-but-not-identical memory pattern is confirmed by the real LLM", func() {
		// Seeded with slightly different (but similar) descriptions, so FindBestMatch's trigram
		// score stays below the high-confidence threshold for each raw expense below, and the
		// Enricher must fall back to the LLM with these as top-N fuzzy candidates.
		Expect(mem.SaveTransactionMapping(ctx, memory.TransactionMappings{
			TransactionPattern: "213 - MAXI 100", Name: ptr("Maxi"), CategoryFK: ptr(realCategoryFK), Income: int64Ptr(0),
		})).To(Succeed())
		Expect(mem.SaveTransactionMapping(ctx, memory.TransactionMappings{
			TransactionPattern: "LIDL OLD", Name: ptr("Lidl"), CategoryFK: ptr(realCategoryFK), Income: int64Ptr(0),
		})).To(Succeed())

		resolver := newRealLLMResolver(ctx)
		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		for _, desc := range []string{"213 - MAXI 779", "LIDL NEW", "214 - MAXI 179"} {
			exp := findExpense(raw, desc)

			result, err := enr.Enrich(ctx, exp, dbLookup{db})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.NeedsUserInput).To(BeFalse(), "the LLM must recognize %q as a known merchant", desc)
			Expect(result.Expense).NotTo(BeNil())
			Expect(result.Expense.CategoryFK).To(HaveValue(Equal(realCategoryFK)))
			Expect(result.Expense.WalletFK).To(HaveValue(Equal(realWalletFK)))
		}
	})

	It("match found in memory: info learned in an earlier session (inserted directly via SQL) resolves the expense without calling the LLM", func() {
		exp := findExpense(raw, "SHOP NB 319")

		// Simulate a mapping learned in an earlier agent session by inserting it directly via
		// SQL (bypassing SaveTransactionMapping), sourced from the "Drinks" transaction
		// (27d54ad1-9137-4cae-ae82-ddb9b10c61ff, see testdata/answers.json's match_found_memory
		// case) in testdata/test-transaction-data.sql. The row is deleted again afterward so it
		// never leaks into another test.
		pool := newRawPool(ctx)
		defer pool.Close()
		_, err := pool.Exec(ctx, `
			INSERT INTO transaction_mappings (
				transaction_pattern, name, category_fk, income, period_length, reoccurrence,
				upcoming_transaction_notification, paid, created_another_future_transaction, skip_paid
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			exp.Description, "Drinks", realCategoryFK, 0, 1, 3, 1, 1, 0, 1)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_, err := pool.Exec(ctx, `DELETE FROM transaction_mappings WHERE transaction_pattern = $1`, exp.Description)
			Expect(err).NotTo(HaveOccurred())
		}()

		enr := handler.NewEnricher(mem, unreachableResolver{}, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense).NotTo(BeNil())
		Expect(result.Expense.Name).To(HaveValue(Equal("Drinks")))
		Expect(result.Expense.CategoryFK).To(HaveValue(Equal(realCategoryFK)))
		Expect(result.Expense.WalletFK).To(HaveValue(Equal(realWalletFK)))
	})

	It("requested from user: the agent asks the user when nothing in memory or the SQLite file matches", func() {
		exp := findExpense(raw, "APOTEKA 139>BEO")

		// Deliberately left empty: memory has no transaction mappings at all, and the SQLite
		// fixture's "note" column is always empty, so no name/note lookup can match either.
		resolver := newRealLLMResolver(ctx)
		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue(), "the agent must ask the user when there is no known match for %q", exp.Description)
		Expect(result.Question).NotTo(BeEmpty())
	})
})

// dbLookup adapts *store.Store to handler.DBLookup.
type dbLookup struct{ s *store.Store }

func (d dbLookup) FindCandidates(ctx context.Context, description string, limit int) ([]handler.DBCandidate, error) {
	cands, err := d.s.FindCandidates(ctx, description, limit)
	if err != nil {
		return nil, err
	}
	out := make([]handler.DBCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, handler.DBCandidate{
			Name:          c.Name,
			CategoryFK:    c.CategoryFK,
			SubCategoryFK: c.SubCategoryFK,
			WalletFK:      c.WalletFK,
			Income:        c.Income,
		})
	}
	return out, nil
}

func (d dbLookup) FindSimilar(ctx context.Context, description string) (handler.DBSimilarFields, bool, error) {
	similar, ok, err := d.s.FindSimilarTransaction(ctx, description)
	if err != nil {
		return handler.DBSimilarFields{}, false, err
	}
	if !ok {
		return handler.DBSimilarFields{}, false, nil
	}
	return handler.DBSimilarFields{
		OriginalDateDue:                 similar.OriginalDateDue,
		PairedTransactionFK:             similar.PairedTransactionFK,
		PeriodLength:                    similar.PeriodLength,
		Reoccurrence:                    similar.Reoccurrence,
		EndDate:                         similar.EndDate,
		UpcomingTransactionNotification: similar.UpcomingTransactionNotification,
		Type:                            similar.Type,
		Paid:                            similar.Paid,
		CreatedAnotherFutureTransaction: similar.CreatedAnotherFutureTransaction,
		SkipPaid:                        similar.SkipPaid,
		ObjectiveFK:                     similar.ObjectiveFK,
		ObjectiveLoanFK:                 similar.ObjectiveLoanFK,
	}, true, nil
}

func ptr(s string) *string    { return &s }
func int64Ptr(v int64) *int64 { return &v }
