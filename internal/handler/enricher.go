package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/rs/zerolog/log"
)

// MemoryStore is the subset of *memory.Store that the Enricher depends on.
type MemoryStore interface {
	FindBestMatch(ctx context.Context, pattern string) (memory.Candidate, bool, error)
	// GetTransactionMapping is an exact (non-fuzzy) lookup by transaction pattern.
	GetTransactionMapping(ctx context.Context, pattern string) (memory.TransactionMappings, bool, error)
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
	ResolveDescription(ctx context.Context, expense llmparser.Expense, candidates []llmparser.Candidate) (llmparser.Resolution, error)
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
	// ResolveCategory resolves a category name (or primary key) from the SQLite categories table.
	// isSubcategory reports whether the match is a subcategory; found is false when nothing matches.
	ResolveCategory(ctx context.Context, input string) (categoryPK string, isSubcategory bool, found bool, err error)
}

const (
	// refundName is the transaction name given to every refund.
	refundName = "Refund"
	// incomeCategoryName is the SQLite category every refund is filed under.
	incomeCategoryName = "Income"
)

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
	loc                     *time.Location // location for zone-less SMS dates; nil means time.Local
}

// WithLocation sets the location used to interpret zone-less SMS dates (nil means time.Local)
// and returns the Enricher for chaining.
func (e *Enricher) WithLocation(loc *time.Location) *Enricher {
	e.loc = loc
	return e
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
		log.Debug().Str("reason", reason).Msg("enrich: parse failed, asking user")
		return Result{NeedsUserInput: true, Question: fmt.Sprintf("I could not parse this message (%s). Could you clarify the transaction details?", reason), Kind: QuestionClarify}, nil
	}

	description := strings.TrimSpace(exp.Description)
	log.Debug().Str("description", description).Str("account", exp.Account).Str("currency", exp.Currency).Msg("enrich: start")
	if description == "" {
		log.Debug().Msg("enrich: empty description, asking user")
		return Result{NeedsUserInput: true, Question: "I could not determine the merchant/counterparty for this transaction. What is it for?", Kind: QuestionClarify}, nil
	}

	var lookup DBLookup
	if len(db) > 0 {
		lookup = db[0]
	}

	walletFK, walletKnown, err := e.resolveWallet(ctx, exp.Account, exp.Currency)
	if err != nil {
		return Result{}, err
	}
	log.Debug().Bool("known", walletKnown).Str("wallet_fk", walletFK).Msg("enrich: wallet resolved")
	if !walletKnown {
		log.Debug().Msg("enrich: unknown wallet, asking user")
		return Result{NeedsUserInput: true, Question: walletQuestion(exp.Account, exp.Currency), Kind: QuestionWallet}, nil
	}

	if exp.Type == llmparser.TransactionRefund {
		log.Debug().Msg("enrich: refund, using Income category")
		return e.enrichRefund(ctx, exp, walletFK, lookup)
	}

	similar, err := lookupSimilar(ctx, description, lookup)
	if err != nil {
		return Result{}, err
	}
	log.Debug().Bool("similar_found", similar != DBSimilarFields{}).Msg("enrich: similar transaction lookup")

	best, ok, err := e.Memory.FindBestMatch(ctx, description)
	if err != nil {
		return Result{}, fmt.Errorf("find best memory match: %w", err)
	}
	log.Debug().Bool("matched", ok && best.Score >= e.HighConfidenceThreshold).Float64("score", best.Score).Float64("threshold", e.HighConfidenceThreshold).Msg("enrich: memory best match")
	if ok && best.Score >= e.HighConfidenceThreshold {
		if (similar == DBSimilarFields{}) && best.Name != nil && *best.Name != "" {
			sim, err := lookupSimilar(ctx, *best.Name, lookup)
			if err != nil {
				log.Debug().Err(err).Str("name", *best.Name).Msg("enrich: similar lookup by memory name failed, ignoring")
			}
			if err == nil && (sim != DBSimilarFields{}) {
				similar = sim
			}
		}
		log.Debug().Msg("enrich: result from memory high-confidence match")
		return Result{Expense: e.buildExpense(exp, best.TransactionMappings, walletFK, similar)}, nil
	}

	candidates, err := e.Memory.TopCandidates(ctx, description, e.TopN)
	if err != nil {
		return Result{}, fmt.Errorf("find top candidates: %w", err)
	}
	log.Debug().Int("count", len(candidates)).Msg("enrich: memory top candidates")

	llmCandidates := toLLMCandidates(candidates, walletFK)
	if len(llmCandidates) == 0 && lookup != nil {
		dbCandidates, err := lookup.FindCandidates(ctx, description, e.TopN)
		if err != nil {
			return Result{}, fmt.Errorf("find db candidates: %w", err)
		}
		log.Debug().Int("count", len(dbCandidates)).Msg("enrich: sqlite fallback candidates")
		llmCandidates = dbToLLMCandidates(dbCandidates)
	}

	resolution, err := e.Resolver.ResolveDescription(ctx, exp, llmCandidates)
	if err != nil {
		return Result{}, fmt.Errorf("resolve description: %w", err)
	}
	log.Debug().Int("candidates", len(llmCandidates)).Bool("matched", resolution.Matched).Bool("needs_user_input", resolution.NeedsUserInput).Int("matched_index", resolution.MatchedIndex).Msg("enrich: llm resolution")

	if resolution.NeedsUserInput || !resolution.Matched {
		log.Debug().Msg("enrich: llm unsure, asking user for name")
		// The question must always quote the exact raw description from the SMS data (description), so
		// we build it ourselves rather than trusting the LLM's freeform question to include it. Name,
		// category and subcategory are then asked one at a time by the caller (see
		// internal/telegram/bot.go processNext/handleNameAnswer/handleCategoryAnswer).
		return Result{NeedsUserInput: true, Question: NameQuestion(description), Kind: QuestionName}, nil
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
	log.Debug().Str("pattern", description).Msg("enrich: transaction mapping saved, expense built")

	return Result{Expense: e.buildExpense(exp, mapping, walletFK, similar)}, nil
}

// ResolveWithUserInput records the user-supplied enrichment fields for a previously ambiguous
// expense into memory and returns the now-complete EnrichedExpense. Any decision made (by the LLM
// or the user) flows back into memory, so the same description/account is recognized automatically
// next time.
func (e *Enricher) ResolveWithUserInput(ctx context.Context, exp llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, income bool, db ...DBLookup) (*EnrichedExpense, error) {
	log.Debug().Str("name", name).Str("category_fk", categoryFK).Str("sub_category_fk", subCategoryFK).Str("wallet_fk", walletFK).Bool("income", income).Str("description", strings.TrimSpace(exp.Description)).Msg("enrich: user input received")
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
		log.Debug().Str("account", account).Str("currency", exp.Currency).Str("wallet_fk", walletFK).Msg("enrich: wallet mapping saved")
	}

	var lookup DBLookup
	if len(db) > 0 {
		lookup = db[0]
	}

	if exp.Type == llmparser.TransactionRefund {
		// A refund is always the fixed refundName/Income transaction: the user's name/category answers
		// do not apply, and memory learns the single refundName row rather than the merchant.
		log.Debug().Msg("enrich: refund from user input, using refund mapping")
		mapping, similar, err := e.refundMapping(ctx, lookup)
		if err != nil {
			return nil, err
		}
		return e.buildExpense(exp, mapping, walletFK, similar), nil
	}

	var similar DBSimilarFields
	if lookup != nil {
		sim, err := lookupSimilar(ctx, name, lookup)
		if err != nil {
			log.Debug().Err(err).Str("name", name).Msg("enrich: similar lookup for user input failed, ignoring")
		}
		if err == nil {
			similar = sim
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
	fillNoSourceFields(&mapping, similar)

	if description != "" {
		if err := e.Memory.SaveTransactionMapping(ctx, mapping); err != nil {
			return nil, fmt.Errorf("save transaction mapping: %w", err)
		}
		log.Debug().Str("pattern", description).Msg("enrich: transaction mapping saved from user input")
	} else {
		log.Debug().Msg("enrich: empty description, transaction mapping save skipped")
	}

	return e.buildExpense(exp, mapping, walletFK, similar), nil
}

// enrichRefund enriches a refund: it is always named refundName, filed under the Income category
// from the SQLite database and counted as income, without consulting the LLM. The merchant
// description only ends up in the expense's Note; see refundMapping for how memory is used.
func (e *Enricher) enrichRefund(ctx context.Context, exp llmparser.Expense, walletFK string, lookup DBLookup) (Result, error) {
	mapping, similar, err := e.refundMapping(ctx, lookup)
	if err != nil {
		return Result{}, err
	}
	return Result{Expense: e.buildExpense(exp, mapping, walletFK, similar)}, nil
}

// refundMapping resolves the single refundName memory row shared by every refund. It starts from
// the cached row (exact lookup on refundName, never fuzzy and never the merchant description),
// tops up any still-nil no-source field from a similar SQLite transaction found by refundName,
// forces Name/CategoryFK/Income to the refund values and saves the merged row back to memory, so
// nil fields never wipe cached ones. The merchant description is never written to memory: Note is
// cleared here and set from the SMS description in buildExpense. similar is returned for
// OriginalDateDue, which is always read live from SQLite.
func (e *Enricher) refundMapping(ctx context.Context, lookup DBLookup) (memory.TransactionMappings, DBSimilarFields, error) {
	if lookup == nil {
		return memory.TransactionMappings{}, DBSimilarFields{}, fmt.Errorf("resolve refund: no database to look up the %s category", incomeCategoryName)
	}
	pk, _, found, err := lookup.ResolveCategory(ctx, incomeCategoryName)
	if err != nil {
		return memory.TransactionMappings{}, DBSimilarFields{}, fmt.Errorf("resolve refund: %w", err)
	}
	if !found {
		return memory.TransactionMappings{}, DBSimilarFields{}, fmt.Errorf("resolve refund: %s category not found", incomeCategoryName)
	}
	log.Debug().Str("category_fk", pk).Msg("enrich: refund category resolved")

	mapping, cached, err := e.Memory.GetTransactionMapping(ctx, refundName)
	if err != nil {
		return memory.TransactionMappings{}, DBSimilarFields{}, fmt.Errorf("get refund mapping: %w", err)
	}
	log.Debug().Bool("cached", cached).Msg("enrich: refund memory lookup")

	similar, err := lookupSimilar(ctx, refundName, lookup)
	if err != nil {
		return memory.TransactionMappings{}, DBSimilarFields{}, err
	}
	log.Debug().Bool("similar_found", similar != DBSimilarFields{}).Msg("enrich: refund similar transaction lookup")

	mapping.TransactionPattern = refundName
	mapping.Name = strPtr(refundName)
	mapping.Note = nil
	mapping.CategoryFK = strPtr(pk)
	mapping.Income = boolPtr(true)
	fillNoSourceFields(&mapping, similar)

	if err := e.Memory.SaveTransactionMapping(ctx, mapping); err != nil {
		return memory.TransactionMappings{}, DBSimilarFields{}, fmt.Errorf("save refund mapping: %w", err)
	}
	log.Debug().Str("pattern", refundName).Msg("enrich: refund mapping saved")

	return mapping, similar, nil
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

	now := time.Now().Truncate(time.Minute)
	dateCreated, ok := parseExpenseDate(exp.Date, e.loc)
	if !ok {
		log.Warn().Str("date", exp.Date).Msg("enrich: cannot parse expense date, using current time")
		dateCreated = now.Unix()
	}

	return &EnrichedExpense{
		DateFallback:                    !ok,
		PairedTransactionFK:             mapping.PairedTransactionFK,
		Name:                            mapping.Name,
		Amount:                          amount,
		Note:                            strPtrOrNil(exp.Description),
		CategoryFK:                      mapping.CategoryFK,
		SubCategoryFK:                   mapping.SubCategoryFK,
		WalletFK:                        strPtrOrNil(walletFK),
		DateCreated:                     dateCreated,
		DateTimeModified:                now.Unix(),
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
