//go:build integration

// Package enrichment_test contains integration tests for the raw-SMS-to-enriched-expense
// pipeline (internal/handler.Enricher), wired against real backends: a Postgres agent memory
// (internal/memory), a Cashew-shaped SQLite file copied from testdata/test-transaction-data.sql
// (a real, minimal Cashew export), and a real, locally running LLM inference server
// (internal/llmparser.Client). Every case reads its raw input from testdata/test-raw-data.json
// and its expected result from testdata/answers.json. They are excluded from the default `go test ./...`
// run via the "integration" build tag since they require external infrastructure. Run them explicitly with:
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

	"github.com/jackc/pgx/v5"
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

const (
	// rawDataPath is a path to raw SMS data for testing.
	rawDataPath = "testdata/test-raw-data.json"
	// answersPath is a path to expected results. It contains a definition of test cases and connected results.
	answersPath = "testdata/answers.json"
	// sqLiteDatabasePath is a path to a real minimal Cashew SQLite database.
	sqliteDatabasePath = "testdata/test-transaction-data.sql"
)

// defaultTestMemoryDBName is a dedicated Postgres database.
const defaultTestMemoryDBName = "expense_agent_test"

// testDSN builds the Postgres DSN.
func testDSN() string {
	cfgPath := defaultTestConfigPath
	if p := os.Getenv("BUDGET_BOT_TEST_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	Expect(err).NotTo(HaveOccurred(), "load test config from %q", cfgPath)

	dbName := defaultTestMemoryDBName
	if n := os.Getenv("MEMORY_TEST_DBNAME"); n != "" {
		cfg.Memory.DBName = n
	}
	cfg.Memory.DBName = dbName

	return cfg.DSN()
}

// answerExpense mirrors one entry of testdata/answers.json, using the same field names/types as
// handler.EnrichedExpense wherever possible, so assertMatchesAnswer can compare them directly.
type answerExpense struct {
	PairedTransactionFK             *string `json:"paired_transaction_fk"`
	Name                            *string `json:"name"`
	Amount                          float64 `json:"amount"`
	Note                            string  `json:"note"`
	CategoryFK                      string  `json:"category_fk"`
	SubCategoryFK                   *string `json:"sub_category_fk"`
	WalletFK                        string  `json:"wallet_fk"`
	DateCreated                     int64   `json:"date_created"`
	DateTimeModified                int64   `json:"date_time_modified"`
	OriginalDateDue                 *int64  `json:"original_date_due"`
	Income                          int64   `json:"income"`
	PeriodLength                    *int64  `json:"period_length"`
	Reoccurrence                    *int64  `json:"reoccurrence"`
	EndDate                         *int64  `json:"end_date"`
	UpcomingTransactionNotification *int64  `json:"upcoming_transaction_notification"`
	Type                            *int64  `json:"type"`
	Paid                            *int64  `json:"paid"`
	CreatedAnotherFutureTransaction *int64  `json:"created_another_future_transaction"`
	SkipPaid                        *int64  `json:"skip_paid"`
	ObjectiveFK                     *string `json:"objective_fk"`
	ObjectiveLoanFK                 *string `json:"objective_loan_fk"`
}

// answers mirrors the top-level shape of testdata/answers.json: one section per Enricher outcome.
type answers struct {
	ExactMatch        []answerExpense `json:"exact_match"`
	LLMFoundMatch     []answerExpense `json:"llm_found_match"`
	MatchFoundMemory  []answerExpense `json:"match_found_memory"`
	RequestedFromUser []answerExpense `json:"requested_from_user"`
}

// loadAnswers reads testdata/answers.json, the single source of truth every case below is
// verified against.
func loadAnswers() answers {
	data, err := os.ReadFile(answersPath)
	Expect(err).NotTo(HaveOccurred(), "read %s", answersPath)

	var a answers
	Expect(json.Unmarshal(data, &a)).To(Succeed(), "unmarshal %s", answersPath)
	return a
}

// loadRawExpenses reads testdata/test-raw-data.json into llmparser.Expense values.
func loadRawExpenses() []llmparser.Expense {
	data, err := os.ReadFile(rawDataPath)
	Expect(err).NotTo(HaveOccurred(), "read %s", rawDataPath)

	var expenses []llmparser.Expense
	Expect(json.Unmarshal(data, &expenses)).To(Succeed(), "unmarshal %s", rawDataPath)
	return expenses
}

// toRFC3339 parses timestamps in format "2006.01.02 15:04" in Belgrade time and converts them to RFC3339 UTC string.
func toRFC3339(raw string) string {
	loc, err := time.LoadLocation("Europe/Belgrade")
	if err != nil {
		loc = time.FixedZone("CEST", 2*3600)
	}
	if t, err := time.ParseInLocation("2006.01.02 15:04", raw, loc); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return raw
}

// findExpense returns the raw expense whose description exactly matches desc, failing the test
// if testdata/test-raw-data.json no longer contains it. The returned expense's Date is converted
// to RFC3339 (see toRFC3339), so it can be fed directly to the Enricher.
func findExpense(expenses []llmparser.Expense, desc string) llmparser.Expense {
	for _, e := range expenses {
		if e.Description == desc {
			e.Date = toRFC3339(e.Date)
			return e
		}
	}
	Fail(fmt.Sprintf("no raw expense with description %q found in %s", desc, rawDataPath))
	return llmparser.Expense{}
}

// assertMatchesAnswer verifies that result matches every field of ans that comes from
// testdata/answers.json. DateTimeModified is normalized with the expected value from answers.json.
func assertMatchesAnswer(result *handler.EnrichedExpense, ans answerExpense) {
	Expect(result).NotTo(BeNil())

	expectStrPtrEqual(result.PairedTransactionFK, ans.PairedTransactionFK)
	expectStrPtrEqual(result.Name, ans.Name)
	Expect(result.Amount).To(BeNumerically("~", ans.Amount, 0.001))
	expectStrPtrEqual(result.Note, &ans.Note)
	Expect(result.CategoryFK).To(HaveValue(Equal(string(ans.CategoryFK))))
	expectStrPtrEqual(result.SubCategoryFK, ans.SubCategoryFK)
	Expect(result.WalletFK).To(HaveValue(Equal(ans.WalletFK)))
	Expect(result.DateCreated).To(Equal(ans.DateCreated))
	Expect(result.DateTimeModified).To(BeNumerically(">", 0))
	result.DateTimeModified = ans.DateTimeModified
	Expect(result.DateTimeModified).To(Equal(ans.DateTimeModified))
	expectInt64PtrEqual(result.OriginalDateDue, ans.OriginalDateDue)
	expectInt64PtrEqual(result.Income, &ans.Income)
	expectInt64PtrEqual(result.PeriodLength, ans.PeriodLength)
	expectInt64PtrEqual(result.Reoccurrence, ans.Reoccurrence)
	expectInt64PtrEqual(result.EndDate, ans.EndDate)
	expectInt64PtrEqual(result.UpcomingTransactionNotification, ans.UpcomingTransactionNotification)
	expectInt64PtrEqual(result.Type, ans.Type)
	expectInt64PtrEqual(result.Paid, ans.Paid)
	expectInt64PtrEqual(result.CreatedAnotherFutureTransaction, ans.CreatedAnotherFutureTransaction)
	expectInt64PtrEqual(result.SkipPaid, ans.SkipPaid)
	expectStrPtrEqual(result.ObjectiveFK, ans.ObjectiveFK)
	expectStrPtrEqual(result.ObjectiveLoanFK, ans.ObjectiveLoanFK)
}

// expectStrPtrEqual compares a possibly-nil *string against a possibly-nil expected *string.
func expectStrPtrEqual(actual *string, expected *string) {
	if expected == nil {
		Expect(actual).To(BeNil())
		return
	}
	Expect(actual).To(HaveValue(Equal(*expected)))
}

// expectInt64PtrEqual compares a possibly-nil *int64 against a possibly-nil expected *int64.
func expectInt64PtrEqual(actual *int64, expected *int64) {
	if expected == nil {
		Expect(actual).To(BeNil())
		return
	}
	Expect(actual).To(HaveValue(Equal(*expected)))
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

	src, err := os.Open(sqliteDatabasePath)
	Expect(err).NotTo(HaveOccurred(), "open %s", sqliteDatabasePath)
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

// ensureTestDatabase creates the dedicated test database on the PostgreSQL instance if it does not already exist.
func ensureTestDatabase(ctx context.Context) {
	cfgPath := defaultTestConfigPath
	if p := os.Getenv("BUDGET_BOT_TEST_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return
	}

	testDBName := defaultTestMemoryDBName
	if n := os.Getenv("MEMORY_TEST_DBNAME"); n != "" {
		testDBName = n
	}
	if testDBName == cfg.Memory.DBName {
		return
	}

	pool, err := pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		return
	}
	defer pool.Close()

	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", testDBName).Scan(&exists); err != nil {
		return
	}
	if !exists {
		_, _ = pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{testDBName}.Sanitize()))
	}
}

// newRealMemoryStore connects to the dedicated test Postgres database (see testDSN), migrates
// it and truncates the memory tables so each test starts from a clean slate. It fail the test
// when Postgres is not reachable.
func newRealMemoryStore(ctx context.Context) *memory.Store {
	ensureTestDatabase(ctx)

	mem, err := memory.New(ctx, testDSN())
	if err != nil {
		Fail(fmt.Sprintf("postgres not reachable: %v", err))
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
// the Enricher's Resolver. Unlike an earlier version of this suite, it fails the test (rather
// than silently skipping it) when the LLM server is not reachable: a skipped spec still reports
// as passing, which would let the LLM-dependent cases go unexercised without anyone noticing.
// Since this suite requires real infrastructure and is never part of the default `go test ./...`
// run, that trade-off is acceptable here.
func newRealLLMResolver(ctx context.Context) *llmparser.Client {
	cfgPath := defaultTestConfigPath
	if p := os.Getenv("BUDGET_BOT_TEST_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	Expect(err).NotTo(HaveOccurred(), "load test config from %q", cfgPath)

	if !filepath.IsAbs(cfg.LLM.APIKeyFile) && cfg.LLM.APIKeyFile != "" {
		cfg.LLM.APIKeyFile = filepath.Join(filepath.Dir(cfgPath), cfg.LLM.APIKeyFile)
	}

	apiKey, err := cfg.APIKey()
	Expect(err).NotTo(HaveOccurred())

	client := llmparser.NewClient(
		cfg.LLM.BaseURL,
		apiKey,
		cfg.LLM.Model,
		time.Duration(cfg.LLM.TimeoutSeconds)*time.Second,
		cfg.LLM.MaxTokensParse,
		cfg.LLM.MaxTokensResolve,
		cfg.LLM.ReasoningEffort,
	)

	// A short resolution call is enough to confirm the server is actually reachable before
	// running a whole spec against it.
	timeout := time.Duration(cfg.LLM.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := client.ResolveDescription(pingCtx, llmparser.Expense{Description: "ping"}, nil); err != nil {
		Fail(fmt.Sprintf("llm server not reachable, cannot exercise the real LLM path: %v", err))
	}
	return client
}

var _ = Describe("Enricher.Enrich against real memory + SQLite + LLM backends", func() {
	var (
		ctx      context.Context
		mem      *memory.Store
		db       *store.Store
		resolver *llmparser.Client
		raw      []llmparser.Expense
		ans      answers
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		DeferCleanup(cancel)

		mem = newRealMemoryStore(ctx)

		db = newSQLiteFixture()
		DeferCleanup(db.Close)

		resolver = newRealLLMResolver(ctx)

		raw = loadRawExpenses()
		ans = loadAnswers()

		// The wallet mapping is shared by every case below: every entry in
		// testdata/test-raw-data.json/testdata/answers.json uses the same bank_account/wallet_fk,
		// and wallet resolution happens before any memory/LLM lookup for the description, so every
		// case needs it resolved to reach that lookup at all. Both values are read from the
		// fixtures themselves rather than hardcoded, so they can never drift from them.
		Expect(raw).NotTo(BeEmpty())
		Expect(ans.ExactMatch).NotTo(BeEmpty())
		Expect(mem.SaveWalletMapping(ctx, memory.WalletMappings{
			AccountPattern: raw[0].Account,
			Currency:       "RSD",
			Name:           "Main card",
			Type:           "bank",
			WalletFK:       ans.ExactMatch[0].WalletFK,
		})).To(Succeed())
	})

	It("exact match: resolves directly from SQLite database exact match", func() {
		Expect(ans.ExactMatch).To(HaveLen(1))
		want := ans.ExactMatch[0]
		exp := findExpense(raw, want.Note)

		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		assertMatchesAnswer(result.Expense, want)

		// Verify mapping was saved to memory for future lookups.
		saved, ok, err := mem.FindBestMatch(ctx, exp.Description)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(saved.Score).To(BeNumerically(">=", memory.HighConfidenceThreshold))
	})

	It("match found in memory: info learned in an earlier session resolves the expense without user input", func() {
		Expect(ans.MatchFoundMemory).To(HaveLen(1))
		want := ans.MatchFoundMemory[0]
		exp := findExpense(raw, want.Note)

		// Simulate a mapping learned in an earlier agent session by inserting it directly via
		// SQL (bypassing SaveTransactionMapping), sourced entirely from testdata/answers.json's
		// match_found_memory entry. The row is deleted again afterward so it never leaks into
		// another test.
		pool := newRawPool(ctx)
		defer pool.Close()
		_, err := pool.Exec(ctx, `
			INSERT INTO transaction_mappings (
				transaction_pattern, name, category_fk, income, period_length, reoccurrence,
				upcoming_transaction_notification, paid, created_another_future_transaction, skip_paid
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			exp.Description, want.Name, string(want.CategoryFK), want.Income, want.PeriodLength,
			want.Reoccurrence, want.UpcomingTransactionNotification, want.Paid,
			want.CreatedAnotherFutureTransaction, want.SkipPaid)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_, err := pool.Exec(ctx, `DELETE FROM transaction_mappings WHERE transaction_pattern = $1`, exp.Description)
			Expect(err).NotTo(HaveOccurred())
		}()

		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeFalse())
		assertMatchesAnswer(result.Expense, want)
	})

	It("llm found match: raw expenses requiring candidate comparison are resolved by the real LLM", func() {
		Expect(ans.LLMFoundMatch).NotTo(BeEmpty())

		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		for _, want := range ans.LLMFoundMatch {
			exp := findExpense(raw, want.Note)

			result, err := enr.Enrich(ctx, exp, dbLookup{db})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.NeedsUserInput).To(BeFalse(), "the LLM must recognize %q as a known merchant", want.Note)
			assertMatchesAnswer(result.Expense, want)
		}
	})

	It("requested from user: unknown merchant triggers user clarification and resolves with user input", func() {
		Expect(ans.RequestedFromUser).To(HaveLen(1))
		want := ans.RequestedFromUser[0]
		exp := findExpense(raw, want.Note)

		enr := handler.NewEnricher(mem, resolver, 5, memory.HighConfidenceThreshold)

		result, err := enr.Enrich(ctx, exp, dbLookup{db})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.NeedsUserInput).To(BeTrue(), "the agent must ask the user when there is no known match for %q", exp.Description)
		Expect(result.Question).To(Equal(handler.NameQuestion(exp.Description)))

		// The user answers with the name/category/wallet from testdata/answers.json; the
		// resulting expense must match that same fixture.
		resolved, err := enr.ResolveWithUserInput(ctx, exp, *want.Name, string(want.CategoryFK), strVal(want.SubCategoryFK), want.WalletFK, want.Income != 0, dbLookup{db})
		Expect(err).NotTo(HaveOccurred())
		assertMatchesAnswer(resolved, want)
	})
})

// strVal dereferences a possibly-nil string pointer, returning "" when nil.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

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

func ptr(s string) *string { return &s }
