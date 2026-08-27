// Package handler ties together LLM parsing and agent memory: it enriches parsed SMS expenses
// with the fields required by the Cashew schema (category, wallet, name, ...), consulting memory
// first and only falling back to the LLM (and, ultimately, the user) when needed. See
// MEMORY-GUIDELINES.md for the overall flow this package implements.
package handler

// EnrichedExpense holds the fields required to insert a transaction into the Cashew schema,
// derived from a parsed llmparser.Expense plus memory/LLM/user-supplied enrichment data.
type EnrichedExpense struct {
	Name           string
	Amount         float64
	CategoryFK     string
	SubCategoryFK  string
	WalletFK       string
	Income         bool
	RawDescription string
	RawAccount     string
	Date           string
}

// Result is the outcome of Enricher.Enrich: either a ready-to-insert Expense, or a Question that
// must be answered by the user before enrichment can continue.
type Result struct {
	// Expense is set when enrichment succeeded without needing the user.
	Expense *EnrichedExpense
	// NeedsUserInput is true when Question must be asked and the answer fed back via
	// Enricher.ResolveWithUserInput.
	NeedsUserInput bool
	Question       string
}
