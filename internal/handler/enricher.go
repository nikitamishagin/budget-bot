package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
)

// MemoryStore is the subset of *memory.Store that the Enricher depends on.
type MemoryStore interface {
	FindBestMatch(ctx context.Context, pattern string) (memory.Candidate, bool, error)
	TopCandidates(ctx context.Context, pattern string, n int) ([]memory.Candidate, error)
	SaveTransactionMapping(ctx context.Context, mapping memory.TransactionMappings) error
	// ResolveWallet / SaveWalletMapping are keyed by (accountPattern, currency): a single card can
	// produce expenses in several currencies that must map to different wallets.
	ResolveWallet(ctx context.Context, accountPattern, currency string) (memory.WalletMappings, bool, error)
	SaveWalletMapping(ctx context.Context, mapping memory.WalletMappings) error
}

// Resolver asks an LLM to match a parsed expense's description against a short list of memory
// candidates, or to say that the user must be asked for missing information.
type Resolver interface {
	ResolveTopic(ctx context.Context, expense llmparser.Expense, candidates []llmparser.Candidate) (llmparser.Resolution, error)
}

// DBLookup finds fallback enrichment candidates directly in the user's SQLite database file,
// used when memory (Postgres) has no candidates for a description: if no exact match is found in
// memory, the agent falls back to searching the SQLite data file for several best (fuzzy/substring)
// matches.
type DBLookup interface {
	FindCandidates(ctx context.Context, description string, limit int) ([]DBCandidate, error)
	// FindSimilar returns the enrichment fields that have no source in raw SMS data (see
	// DBSimilarFields) for the most recent existing transaction matching description, so the
	// agent can copy them instead of inventing defaults or asking the user (AGENTS.md's field
	// resolution rule). ok is false when no such transaction exists.
	FindSimilar(ctx context.Context, description string) (DBSimilarFields, bool, error)
}

// DBCandidate is candidate enrichment (name/category/wallet/income) found directly in the
// user's SQLite database file.
type DBCandidate struct {
	Name          string
	CategoryFK    string
	SubCategoryFK string
	WalletFK      string
	Income        bool
}

// DBSimilarFields holds the transaction fields that have no source in raw SMS data and are never
// supplied by the user (see AGENTS.md's field resolution rule): they must be copied from a
// similar existing SQLite transaction, and left nil when no such transaction exists.
type DBSimilarFields struct {
	OriginalDateDue                 *int64
	PairedTransactionFK             *string
	PeriodLength                    *int64
	Reoccurrence                    *int64
	EndDate                         *int64
	UpcomingTransactionNotification *int64
	Type                            *int64
	Paid                            *int64
	CreatedAnotherFutureTransaction *int64
	SkipPaid                        *int64
	ObjectiveFK                     *string
	ObjectiveLoanFK                 *string
}

// Enricher enriches parsed expenses with the fields required by the Cashew schema, consulting
// memory first and only calling the LLM/user when needed.
type Enricher struct {
	Memory                  MemoryStore
	Resolver                Resolver
	TopN                    int
	HighConfidenceThreshold float64
}

// NewEnricher creates an Enricher with the given memory store and LLM resolver.
func NewEnricher(store MemoryStore, resolver Resolver, topN int, highConfidenceThreshold float64) *Enricher {
	if topN <= 0 {
		topN = memory.DefaultTopN
	}
	if highConfidenceThreshold <= 0 {
		highConfidenceThreshold = memory.HighConfidenceThreshold
	}
	return &Enricher{
		Memory:                  store,
		Resolver:                resolver,
		TopN:                    topN,
		HighConfidenceThreshold: highConfidenceThreshold,
	}
}

// Enrich turns a parsed llmparser.Expense into an EnrichedExpense ready to insert, or into a
// Result asking the user for missing information. It never calls the LLM when a high-confidence
// memory match already exists, and it never passes more than TopN candidates to the LLM. An
// optional DBLookup (db) is consulted as a fallback candidate source, when memory has none, and
// as the source of the no-source enrichment fields (see DBSimilarFields).
func (e *Enricher) Enrich(ctx context.Context, exp llmparser.Expense, db ...DBLookup) (Result, error) {
	if exp.ParseFailed {
		reason := exp.FailureReason
		if reason == "" {
			reason = "the message could not be parsed"
		}
		return Result{NeedsUserInput: true, Question: fmt.Sprintf("I could not parse this message (%s). Could you clarify the transaction details?", reason)}, nil
	}

	description := strings.TrimSpace(exp.Description)
	if description == "" {
		return Result{NeedsUserInput: true, Question: "I could not determine the merchant/counterparty for this transaction. What is it for?"}, nil
	}

	walletFK, walletKnown, err := e.resolveWallet(ctx, exp.Account, exp.Currency)
	if err != nil {
		return Result{}, err
	}
	if !walletKnown {
		return Result{NeedsUserInput: true, Question: walletQuestion(exp.Account, exp.Currency)}, nil
	}

	var lookup DBLookup
	if len(db) > 0 {
		lookup = db[0]
	}
	similar, err := lookupSimilar(ctx, description, lookup)
	if err != nil {
		return Result{}, err
	}

	best, ok, err := e.Memory.FindBestMatch(ctx, description)
	if err != nil {
		return Result{}, fmt.Errorf("find best memory match: %w", err)
	}
	if ok && best.Score >= e.HighConfidenceThreshold {
		return Result{Expense: e.buildExpense(exp, best.TransactionMappings, walletFK, similar)}, nil
	}

	candidates, err := e.Memory.TopCandidates(ctx, description, e.TopN)
	if err != nil {
		return Result{}, fmt.Errorf("find top candidates: %w", err)
	}

	llmCandidates := toLLMCandidates(candidates, walletFK)
	if len(llmCandidates) == 0 && lookup != nil {
		dbCandidates, err := lookup.FindCandidates(ctx, description, e.TopN)
		if err != nil {
			return Result{}, fmt.Errorf("find db candidates: %w", err)
		}
		llmCandidates = dbToLLMCandidates(dbCandidates)
	}

	resolution, err := e.Resolver.ResolveTopic(ctx, exp, llmCandidates)
	if err != nil {
		return Result{}, fmt.Errorf("resolve description: %w", err)
	}

	if resolution.NeedsUserInput || !resolution.Matched {
		// The question must always quote the exact raw description from the SMS data (description), so
		// we build it ourselves rather than trusting the LLM's freeform question to include it. Name,
		// category and subcategory are then asked one at a time by the caller (see
		// internal/telegram/bot.go processNext/handleNameAnswer/handleCategoryAnswer).
		return Result{NeedsUserInput: true, Question: NameQuestion(description)}, nil
	}

	mapping := memory.TransactionMappings{
		TransactionPattern: description,
		Name:               strPtr(resolution.Name),
		CategoryFK:         strPtr(resolution.CategoryFK),
		SubCategoryFK:      strPtrOrNil(resolution.SubCategoryFK),
		Income:             boolPtr(resolution.Income),
	}
	if resolution.MatchedIndex >= 0 && resolution.MatchedIndex < len(llmCandidates) {
		chosen := llmCandidates[resolution.MatchedIndex]
		mapping.Name = strPtr(chosen.Name)
		mapping.CategoryFK = strPtr(chosen.CategoryFK)
		mapping.SubCategoryFK = strPtrOrNil(chosen.SubCategoryFK)
		mapping.Income = boolPtr(chosen.Income)
	}
	fillNoSourceFields(&mapping, similar)

	if err := e.Memory.SaveTransactionMapping(ctx, mapping); err != nil {
		return Result{}, fmt.Errorf("save transaction mapping: %w", err)
	}

	return Result{Expense: e.buildExpense(exp, mapping, walletFK, similar)}, nil
}

// ResolveWithUserInput records the user-supplied enrichment fields for a previously ambiguous
// expense into memory and returns the now-complete EnrichedExpense. Any decision made (by the LLM
// or the user) flows back into memory, so the same description/account is recognized automatically
// next time. The fields with no source in raw SMS data (see DBSimilarFields) are left nil here,
// since no SQLite database is consulted at this point; they stay NULL, per AGENTS.md's field
// resolution rule, unless a later high-confidence memory hit already has them cached.
func (e *Enricher) ResolveWithUserInput(ctx context.Context, exp llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, income bool) (*EnrichedExpense, error) {
	if categoryFK == "" || walletFK == "" {
		return nil, fmt.Errorf("category_fk and wallet_fk are required")
	}

	if account := strings.TrimSpace(exp.Account); account != "" {
		if err := e.Memory.SaveWalletMapping(ctx, memory.WalletMappings{
			AccountPattern: account,
			Currency:       exp.Currency,
			WalletFK:       walletFK,
		}); err != nil {
			return nil, fmt.Errorf("save wallet mapping: %w", err)
		}
	}

	description := strings.TrimSpace(exp.Description)
	mapping := memory.TransactionMappings{
		TransactionPattern: description,
		Name:               strPtr(name),
		CategoryFK:         strPtr(categoryFK),
		SubCategoryFK:      strPtrOrNil(subCategoryFK),
		Income:             boolPtr(income),
	}
	if description != "" {
		if err := e.Memory.SaveTransactionMapping(ctx, mapping); err != nil {
			return nil, fmt.Errorf("save transaction mapping: %w", err)
		}
	}

	return e.buildExpense(exp, mapping, walletFK, DBSimilarFields{}), nil
}

// resolveWallet resolves a bank account/card identifier and currency from the SMS to a wallet,
// using the static wallet_mappings memory table. Currency is part of the lookup key because a
// single card can produce expenses in several currencies, each mapped to a different wallet.
func (e *Enricher) resolveWallet(ctx context.Context, account, currency string) (walletFK string, ok bool, err error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return "", false, nil
	}
	wallet, ok, err := e.Memory.ResolveWallet(ctx, account, currency)
	if err != nil {
		return "", false, fmt.Errorf("resolve wallet: %w", err)
	}
	if !ok {
		return "", false, nil
	}
	return wallet.WalletFK, true, nil
}

// lookupSimilar consults lookup (when not nil) for the no-source enrichment fields of a similar
// existing SQLite transaction, per AGENTS.md's field resolution rule. It returns a zero
// DBSimilarFields (all nil) when lookup is nil or no similar transaction exists.
func lookupSimilar(ctx context.Context, description string, lookup DBLookup) (DBSimilarFields, error) {
	if lookup == nil {
		return DBSimilarFields{}, nil
	}
	similar, ok, err := lookup.FindSimilar(ctx, description)
	if err != nil {
		return DBSimilarFields{}, fmt.Errorf("find similar transaction: %w", err)
	}
	if !ok {
		return DBSimilarFields{}, nil
	}
	return similar, nil
}

// fillNoSourceFields fills any still-nil no-source field of mapping (see AGENTS.md's field
// resolution rule) from similar, so a freshly learned mapping caches them in memory for next time.
func fillNoSourceFields(mapping *memory.TransactionMappings, similar DBSimilarFields) {
	if mapping.PairedTransactionFK == nil {
		mapping.PairedTransactionFK = similar.PairedTransactionFK
	}
	if mapping.PeriodLength == nil {
		mapping.PeriodLength = similar.PeriodLength
	}
	if mapping.Reoccurrence == nil {
		mapping.Reoccurrence = similar.Reoccurrence
	}
	if mapping.EndDate == nil {
		mapping.EndDate = similar.EndDate
	}
	if mapping.UpcomingTransactionNotification == nil {
		mapping.UpcomingTransactionNotification = similar.UpcomingTransactionNotification
	}
	if mapping.Type == nil {
		mapping.Type = similar.Type
	}
	if mapping.Paid == nil {
		mapping.Paid = similar.Paid
	}
	if mapping.CreatedAnotherFutureTransaction == nil {
		mapping.CreatedAnotherFutureTransaction = similar.CreatedAnotherFutureTransaction
	}
	if mapping.SkipPaid == nil {
		mapping.SkipPaid = similar.SkipPaid
	}
	if mapping.ObjectiveFK == nil {
		mapping.ObjectiveFK = similar.ObjectiveFK
	}
	if mapping.ObjectiveLoanFK == nil {
		mapping.ObjectiveLoanFK = similar.ObjectiveLoanFK
	}
}

// NameQuestion builds the strictly formatted question asked when the agent cannot confidently
// determine an expense's name/category/subcategory: it must always quote the exact raw
// description from the SMS data. Category and subcategory are asked separately afterward, see
// CategoryQuestion and SubCategoryQuestion.
func NameQuestion(rawDesc string) string {
	return fmt.Sprintf("What is the name of the %q expense?", rawDesc)
}

// CategoryQuestion builds the question asked after the name, for the same raw description.
func CategoryQuestion(rawDesc string) string {
	return fmt.Sprintf("What category does the %q expense belong to?", rawDesc)
}

// SubCategoryQuestion builds the question asked after the category, for the same raw
// description. The subcategory is optional, so the caller should accept an empty/skip answer.
func SubCategoryQuestion(rawDesc string) string {
	return fmt.Sprintf("What subcategory does the %q expense belong to? (reply \"-\" to leave it empty)", rawDesc)
}

// walletQuestion builds the question asked when the agent cannot resolve a bank account/card
// number (and currency) to a known wallet: the currency is included since the same card can map
// to different wallets depending on it (e.g., wallets named "TBC * (RSD)" vs. "TBC * (USD)").
func walletQuestion(rawAccount, currency string) string {
	if strings.TrimSpace(currency) == "" {
		return fmt.Sprintf("Which wallet does account %q belong to?", rawAccount)
	}
	return fmt.Sprintf("Which wallet does account %q in %s belong to?", rawAccount, currency)
}

// buildExpense assembles the final EnrichedExpense: name/category/subcategory/income and the
// no-source fields come from mapping (a memory.TransactionMappings, already learned or just
// resolved), OriginalDateDue always comes fresh from similar (see AGENTS.md: it must reflect
// whatever similar transaction currently exists, not a cached value), and Amount is signed
// according to Cashew's convention (expenses negative, income positive).
func (e *Enricher) buildExpense(exp llmparser.Expense, mapping memory.TransactionMappings, walletFK string, similar DBSimilarFields) *EnrichedExpense {
	income := boolVal(mapping.Income) || exp.Type == llmparser.TransactionIncome
	amount := exp.Amount
	if !income {
		amount = -amount
	}

	return &EnrichedExpense{
		PairedTransactionFK:             mapping.PairedTransactionFK,
		Name:                            mapping.Name,
		Amount:                          amount,
		Note:                            strPtrOrNil(exp.Description),
		CategoryFK:                      mapping.CategoryFK,
		SubCategoryFK:                   mapping.SubCategoryFK,
		WalletFK:                        strPtrOrNil(walletFK),
		DateCreated:                     parseExpenseDate(exp.Date),
		DateTimeModified:                time.Now().Unix(),
		OriginalDateDue:                 similar.OriginalDateDue,
		Income:                          boolPtr(income),
		PeriodLength:                    mapping.PeriodLength,
		Reoccurrence:                    mapping.Reoccurrence,
		EndDate:                         mapping.EndDate,
		UpcomingTransactionNotification: mapping.UpcomingTransactionNotification,
		Type:                            mapping.Type,
		Paid:                            mapping.Paid,
		CreatedAnotherFutureTransaction: mapping.CreatedAnotherFutureTransaction,
		SkipPaid:                        mapping.SkipPaid,
		ObjectiveFK:                     mapping.ObjectiveFK,
		ObjectiveLoanFK:                 mapping.ObjectiveLoanFK,
	}
}

// parseExpenseDate parses a llmparser.Expense.Date (RFC3339 "if determinable, otherwise as-is")
// into a second-epoch timestamp (Cashew's convention), falling back to now when it cannot be
// parsed.
func parseExpenseDate(date string) int64 {
	if t, err := time.Parse(time.RFC3339, date); err == nil {
		return t.Unix()
	}
	return time.Now().Unix()
}

func toLLMCandidates(candidates []memory.Candidate, walletFK string) []llmparser.Candidate {
	out := make([]llmparser.Candidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, llmparser.Candidate{
			Name:          strVal(c.Name),
			CategoryFK:    strVal(c.CategoryFK),
			SubCategoryFK: strVal(c.SubCategoryFK),
			WalletFK:      walletFK,
			Income:        boolVal(c.Income),
		})
	}
	return out
}

func dbToLLMCandidates(candidates []DBCandidate) []llmparser.Candidate {
	out := make([]llmparser.Candidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, llmparser.Candidate{
			Name:          c.Name,
			CategoryFK:    c.CategoryFK,
			SubCategoryFK: c.SubCategoryFK,
			WalletFK:      c.WalletFK,
			Income:        c.Income,
		})
	}
	return out
}

// strPtr returns a pointer to s, used to fill the nullable string fields of
// memory.TransactionMappings.
func strPtr(s string) *string {
	return &s
}

// strPtrOrNil returns nil for an empty string, otherwise a pointer to it: used for the optional
// sub_category_fk field, which should stay NULL rather than an empty string when not set.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// strVal dereferences a possibly-nil string pointer, returning "" when nil.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// boolPtr returns a pointer to the int64 representation of b (1 for true, 0 for false), used to
// fill the nullable income field of memory.TransactionMappings.
func boolPtr(b bool) *int64 {
	var v int64
	if b {
		v = 1
	}
	return &v
}

// boolVal dereferences a possibly-nil int64 pointer, returning false when nil or 0.
func boolVal(v *int64) bool {
	return v != nil && *v != 0
}
