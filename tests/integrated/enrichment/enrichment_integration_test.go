//go:build integration

// Package enrichment_test contains integration tests for the raw-SMS-to-enriched-expense
// pipeline (internal/handler.Enricher), wired against real backends: a Postgres agent memory
// (internal/memory) and a Cashew-shaped SQLite file built from testdata/test-transaction-data.csv,
// plus a real, locally running LLM inference server (internal/llmparser.Client) for the cases
// that require it. They are excluded from the default `go test ./...` run via the "integration"
// build tag, since they require external infrastructure. Run them explicitly with:
//
//	go test -tags=integration ./tests/integrated/enrichment/...
//
// Both Postgres and LLM connection settings are taken from the standard config file (see
// config.example.yaml), overridable via BUDGET_BOT_TEST_CONFIG; the suite always connects to a
// dedicated test database (see defaultTestMemoryDBName), never the configured production one.
package enrichment_test

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"

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

// rawDataPath and csvDataPath point at the test fixtures for this suite, kept under testdata/
// (a Go-conventional directory name that the toolchain never treats as a package) rather than
// the repository's ./data/ directory, which is only a scratch/working directory for the running
// agent, not a place for checked-in test fixtures.
const (
	rawDataPath = "testdata/test-raw-data.json"
	csvDataPath = "testdata/test-transaction-data.csv"
)

// realWalletFK/realCategoryFK are taken directly from testdata/test-transaction-data.csv: they
// are the actual wallet and category the pre-existing "Maxi" transactions use, so seeded memory
// mappings resolve to values that also make sense against the SQLite fixture built from that CSV.
// testAccount is the bank_account value shared by every entry in testdata/test-raw-data.json.
const (
	realWalletFK   = "7f9456f5-0e24-4eae-b9bd-2b031d667a1d"
	realCategoryFK = "2"
	testAccount    = "1391"
)

// defaultTestMemoryDBName is a dedicated Postgres database, separate from the memory.dbname
// configured for the running agent, so this suite's TRUNCATE-heavy setup can never touch real
// agent memory. Override via MEMORY_TEST_DBNAME (e.g. to point at a CI-provisioned database).
const defaultTestMemoryDBName = "expense_agent_test"

// testDSN builds the Postgres DSN for this suite: connection details (host, port, user,
// password, sslmode) come from the standard config file (see config.example.yaml, overridable
// via BUDGET_BOT_TEST_CONFIG) so no credentials are ever hardcoded here, but the database name
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
// if data/test-raw-data.json no longer contains it.
func findExpense(expenses []llmparser.Expense, desc string) llmparser.Expense {
	for _, e := range expenses {
		if e.Description == desc {
			return e
		}
	}
	Fail(fmt.Sprintf("no raw expense with description %q found in %s", desc, rawDataPath))
	return llmparser.Expense{}
}

// sqliteSchema mirrors internal/store/store_test.go's testSchema: the minimal Cashew-shaped
// schema internal/store.Store requires (see internal/store/store.go's requiredTables).
const sqliteSchema = `
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

// csv column indexes into a testdata/test-transaction-data.csv row, following the column order
// of the Cashew "transactions" table as documented in AGENTS.md.
const (
	csvColTransactionPK = 0
	csvColName          = 2
	csvColAmount        = 3
	csvColNote          = 4
	csvColCategoryFK    = 5
	csvColSubCategoryFK = 6
	csvColWalletFK      = 7
	csvColDateCreated   = 8
	csvColIncome        = 11
)

// scratchDataDir is the repository's ./data directory: a gitignored scratch area for temporary
// runtime files (the running agent's SQLite files, screenshots, etc). Any temporary file this
// suite creates (the seeded SQLite fixture) belongs there too, not under os.TempDir().
const scratchDataDir = "../../../data"

// newSQLiteFixture builds a temporary Cashew-shaped SQLite database file, seeded from
// testdata/test-transaction-data.csv, and returns an opened *store.Store plus the number of
// rows seeded (for sanity-checking the fixture).
func newSQLiteFixture() (*store.Store, int) {
	dir, err := os.MkdirTemp(scratchDataDir, "enrichment-integration-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "cashew.sqlite")

	raw, err := sql.Open("sqlite", path)
	Expect(err).NotTo(HaveOccurred())
	_, err = raw.Exec(sqliteSchema)
	Expect(err).NotTo(HaveOccurred())

	n := seedFromCSV(raw)
	Expect(raw.Close()).To(Succeed())

	s, err := store.Open(path)
	Expect(err).NotTo(HaveOccurred(), "open seeded sqlite fixture")
	return s, n
}

// seedFromCSV parses csvDataPath (using encoding/csv, since several rows contain quoted,
// multi-line note fields) and inserts every row into the transactions table, returning the
// number of rows inserted.
func seedFromCSV(db *sql.DB) int {
	f, err := os.Open(csvDataPath)
	Expect(err).NotTo(HaveOccurred(), "open %s", csvDataPath)
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	n := 0
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		Expect(err).NotTo(HaveOccurred(), "parse %s", csvDataPath)

		amount, err := strconv.ParseFloat(record[csvColAmount], 64)
		Expect(err).NotTo(HaveOccurred(), "parse amount in row %v", record)
		dateCreated, err := strconv.ParseInt(record[csvColDateCreated], 10, 64)
		Expect(err).NotTo(HaveOccurred(), "parse date_created in row %v", record)

		var subCategory sql.NullString
		if record[csvColSubCategoryFK] != "" {
			subCategory = sql.NullString{String: record[csvColSubCategoryFK], Valid: true}
		}
		var income int
		if record[csvColIncome] == "1" {
			income = 1
		}

		_, err = db.Exec(`
			INSERT INTO transactions (
				transaction_pk, name, amount, note, category_fk, sub_category_fk, wallet_fk,
				date_created, income, paid, skip_paid
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0)`,
			record[csvColTransactionPK], record[csvColName], amount, record[csvColNote],
			record[csvColCategoryFK], subCategory, record[csvColWalletFK], dateCreated, income,
		)
		Expect(err).NotTo(HaveOccurred(), "insert row %v", record)
		n++
	}
	return n
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

// truncateMemoryTables wipes the memory tables so each test starts from a clean slate. It uses
// its own pgxpool connection because *memory.Store does not expose its underlying pool.
func truncateMemoryTables(ctx context.Context) {
	pool, err := pgxpool.New(ctx, testDSN())
	Expect(err).NotTo(HaveOccurred())
	defer pool.Close()

	_, err = pool.Exec(ctx, "TRUNCATE transaction_mappings, wallet_mappings")
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
		ctx     context.Context
		mem     *memory.Store
		db      *store.Store
		raw     []llmparser.Expense
		numRows int
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		mem = newRealMemoryStore(ctx)

		db, numRows = newSQLiteFixture()
		DeferCleanup(db.Close)
		Expect(numRows).To(BeNumerically(">", 0), "sqlite fixture must be seeded from the CSV")

		raw = loadRawExpenses()

		// The wallet mapping is shared by all three cases below: the raw data always uses the
		// same bank_account ("1391"), and wallet resolution happens before any memory/LLM lookup
		// for the description, so every case needs it resolved to reach that lookup at all.
		Expect(mem.SaveWalletMapping(ctx, memory.WalletMappings{
			AccountPattern: testAccount,
			Currency:       "RSD",
			Name:           "Main card",
			Type:           "bank",
			WalletFK:       realWalletFK,
		})).To(Succeed())
	})

	It("exact match: resolves directly from an exact memory pattern, without calling the LLM", func() {
		exp := findExpense(raw, "213 - MAXI 779")

		Expect(mem.SaveTransactionMapping(ctx, memory.TransactionMappings{
			TransactionPattern: exp.Description,
			Name:               ptr("Maxi"),
			CategoryFK:         ptr(realCategoryFK),
			Income:             int64Ptr(0),
		})).To(Succeed())

		enr := handler.NewEnricher(mem, unreachableResolver{}, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		Expect(result.Expense).NotTo(BeNil())
		Expect(result.Expense.Name).To(Equal("Maxi"))
		Expect(result.Expense.CategoryFK).To(Equal(realCategoryFK))
		Expect(result.Expense.WalletFK).To(Equal(realWalletFK))
		Expect(result.Expense.Amount).To(Equal(exp.Amount))
	})

	It("partial match: a similar-but-not-identical memory pattern is confirmed by the real LLM", func() {
		exp := findExpense(raw, "214 - MAXI 179")

		// Seeded with a different (but similar) description, so FindBestMatch's trigram score
		// stays below the high-confidence threshold and the Enricher must fall back to the LLM
		// with this as one of the top-N fuzzy candidates.
		Expect(mem.SaveTransactionMapping(ctx, memory.TransactionMappings{
			TransactionPattern: "213 - MAXI 779",
			Name:               ptr("Maxi"),
			CategoryFK:         ptr(realCategoryFK),
			Income:             int64Ptr(0),
		})).To(Succeed())

		best, ok, err := mem.FindBestMatch(ctx, exp.Description)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(best.Score).To(BeNumerically("<", memory.HighConfidenceThreshold), "fixture must exercise the partial-match path, not the exact one")

		resolver := newRealLLMResolver(ctx)
		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse(), "the LLM must recognize %q as the same merchant as %q", exp.Description, "213 - MAXI 779")
		Expect(result.Expense).NotTo(BeNil())
		Expect(result.Expense.CategoryFK).To(Equal(realCategoryFK))
		Expect(result.Expense.WalletFK).To(Equal(realWalletFK))
	})

	It("no match: the agent asks the user when nothing in memory or the SQLite file matches", func() {
		exp := findExpense(raw, "KAFETERIJA SANK")

		// Deliberately left empty: memory has no transaction mappings at all, and the SQLite
		// fixture's "note" column is always empty so no name/note lookup can match either.
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

func ptr(s string) *string    { return &s }
func int64Ptr(v int64) *int64 { return &v }
