package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
)

// MemoryStore is the subset of *memory.Store that the Enricher depends on.
type MemoryStore interface {
	FindBestMatch(ctx context.Context, topic string) (memory.Candidate, bool, error)
	TopCandidates(ctx context.Context, topic string, n int) ([]memory.Candidate, error)
	SaveTrainingPair(ctx context.Context, pair memory.TrainingPair) error
	ResolveAccount(ctx context.Context, accountPattern string) (string, bool, error)
	SaveAccountMapping(ctx context.Context, accountPattern, walletFK string) error
}

// Resolver asks an LLM to match a parsed expense's topic against a short list of memory
// candidates, or to say that the user must be asked for missing information.
type Resolver interface {
	ResolveTopic(ctx context.Context, expense llmparser.Expense, candidates []llmparser.Candidate) (llmparser.Resolution, error)
}

// DBLookup finds fallback enrichment candidates directly in the user's SQLite database file,
// used when memory (Postgres) has no candidates for a topic - see AGENTS.md step 8 of the main
// loop: "If the agent doesn't find something [in memory], it attempts to find matches in the
// SQLite data file [...] it gets several best matches (fuzzy/substring)".
type DBLookup interface {
	FindCandidates(ctx context.Context, topic string, limit int) ([]DBCandidate, error)
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

// Enricher enriches parsed expenses with the fields required by the Cashew schema, consulting
// memory first (per MEMORY-GUIDELINES.md) and only calling the LLM/user when needed.
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
// optional DBLookup (db) is consulted as a fallback candidate source, when memory has none, per
// AGENTS.md step 8.
func (e *Enricher) Enrich(ctx context.Context, exp llmparser.Expense, db ...DBLookup) (Result, error) {
	if exp.ParseFailed {
		reason := exp.FailureReason
		if reason == "" {
			reason = "the message could not be parsed"
		}
		return Result{NeedsUserInput: true, Question: fmt.Sprintf("I could not parse this message (%s). Could you clarify the transaction details?", reason)}, nil
	}

	topic := strings.TrimSpace(exp.Topic)
	if topic == "" {
		return Result{NeedsUserInput: true, Question: "I could not determine the merchant/counterparty for this transaction. What is it for?"}, nil
	}

	walletFK, walletKnown, err := e.resolveWallet(ctx, exp.Account)
	if err != nil {
		return Result{}, err
	}

	best, ok, err := e.Memory.FindBestMatch(ctx, topic)
	if err != nil {
		return Result{}, fmt.Errorf("find best memory match: %w", err)
	}
	if ok && best.Score >= e.HighConfidenceThreshold {
		wallet := walletFK
		if !walletKnown {
			wallet = best.WalletFK
		}
		return Result{Expense: e.buildExpense(exp, best.TrainingPair, wallet)}, nil
	}

	if !walletKnown {
		return Result{NeedsUserInput: true, Question: fmt.Sprintf("Which wallet does account %q belong to?", exp.Account)}, nil
	}

	candidates, err := e.Memory.TopCandidates(ctx, topic, e.TopN)
	if err != nil {
		return Result{}, fmt.Errorf("find top candidates: %w", err)
	}

	llmCandidates := toLLMCandidates(candidates)
	if len(llmCandidates) == 0 && len(db) > 0 && db[0] != nil {
		dbCandidates, err := db[0].FindCandidates(ctx, topic, e.TopN)
		if err != nil {
			return Result{}, fmt.Errorf("find db candidates: %w", err)
		}
		llmCandidates = dbToLLMCandidates(dbCandidates)
	}

	resolution, err := e.Resolver.ResolveTopic(ctx, exp, llmCandidates)
	if err != nil {
		return Result{}, fmt.Errorf("resolve topic: %w", err)
	}

	if resolution.NeedsUserInput || !resolution.Matched {
		question := resolution.Question
		if question == "" {
			question = fmt.Sprintf("What category and wallet should I use for %q?", topic)
		}
		return Result{NeedsUserInput: true, Question: question}, nil
	}

	pair := memory.TrainingPair{
		RawTopic:      topic,
		Name:          resolution.Name,
		CategoryFK:    resolution.CategoryFK,
		SubCategoryFK: resolution.SubCategoryFK,
		WalletFK:      walletFK,
		Income:        resolution.Income,
	}
	if resolution.MatchedIndex >= 0 && resolution.MatchedIndex < len(llmCandidates) {
		chosen := llmCandidates[resolution.MatchedIndex]
		pair.Name = chosen.Name
		pair.CategoryFK = chosen.CategoryFK
		pair.SubCategoryFK = chosen.SubCategoryFK
		pair.Income = chosen.Income
	}

	if err := e.Memory.SaveTrainingPair(ctx, pair); err != nil {
		return Result{}, fmt.Errorf("save training pair: %w", err)
	}

	return Result{Expense: e.buildExpense(exp, pair, walletFK)}, nil
}

// ResolveWithUserInput records the user-supplied enrichment fields for a previously ambiguous
// expense into memory and returns the now-complete EnrichedExpense. This closes the loop
// described in MEMORY-GUIDELINES.md: any decision made (by the LLM or the user) flows back into
// memory, so the same topic/account is recognized automatically next time.
func (e *Enricher) ResolveWithUserInput(ctx context.Context, exp llmparser.Expense, name, categoryFK, subCategoryFK, walletFK string, income bool) (*EnrichedExpense, error) {
	if categoryFK == "" || walletFK == "" {
		return nil, fmt.Errorf("category_fk and wallet_fk are required")
	}

	if account := strings.TrimSpace(exp.Account); account != "" {
		if err := e.Memory.SaveAccountMapping(ctx, account, walletFK); err != nil {
			return nil, fmt.Errorf("save account mapping: %w", err)
		}
	}

	topic := strings.TrimSpace(exp.Topic)
	pair := memory.TrainingPair{
		RawTopic:      topic,
		Name:          name,
		CategoryFK:    categoryFK,
		SubCategoryFK: subCategoryFK,
		WalletFK:      walletFK,
		Income:        income,
	}
	if topic != "" {
		if err := e.Memory.SaveTrainingPair(ctx, pair); err != nil {
			return nil, fmt.Errorf("save training pair: %w", err)
		}
	}

	return e.buildExpense(exp, pair, walletFK), nil
}

// resolveWallet resolves a bank account/card identifier from the SMS to a wallet, using the
// static bank_account_mappings memory table.
func (e *Enricher) resolveWallet(ctx context.Context, account string) (walletFK string, ok bool, err error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return "", false, nil
	}
	walletFK, ok, err = e.Memory.ResolveAccount(ctx, account)
	if err != nil {
		return "", false, fmt.Errorf("resolve account: %w", err)
	}
	return walletFK, ok, nil
}

func (e *Enricher) buildExpense(exp llmparser.Expense, pair memory.TrainingPair, walletFK string) *EnrichedExpense {
	return &EnrichedExpense{
		Name:          pair.Name,
		Amount:        exp.Amount,
		CategoryFK:    pair.CategoryFK,
		SubCategoryFK: pair.SubCategoryFK,
		WalletFK:      walletFK,
		Income:        pair.Income || exp.Type == llmparser.TransactionIncome,
		RawTopic:      exp.Topic,
		RawAccount:    exp.Account,
		Date:          exp.Date,
	}
}

func toLLMCandidates(candidates []memory.Candidate) []llmparser.Candidate {
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
