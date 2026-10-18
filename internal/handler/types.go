// Package handler ties together LLM parsing and agent memory: it enriches parsed SMS expenses
// with the fields required by the Cashew schema (category, wallet, name, ...), consulting memory
// first and only falling back to the LLM (and, ultimately, the user) when needed.
package handler

// EnrichedExpense holds the fields required to insert a transaction into the Cashew schema,
// derived from a parsed llmparser.Expense plus memory/LLM/user-supplied enrichment data.
type EnrichedExpense struct {
	PairedTransactionFK *string `db:"paired_transaction_fk"` // The key defining which transaction is paired with another one. Used only for transfers from one wallet to another.
	// Name of transaction. Can be found in the "associated_titles" table (key title) of the SQLite database, or in the agent's memory.
	// If not found an exact match, resolution can be requested from llm (ResolveDescription) or from the user (ResolveWithUserInput).
	Name                            *string `db:"name"`
	Amount                          float64 `db:"amount"`             // The transaction amount. Attention: expenses are always negative, income is positive!
	Note                            *string `db:"note"`               // The raw description of the merchant/counterparty from the SMS data.
	CategoryFK                      *string `db:"category_fk"`        // The category foreign key. Can be found in the "categories" table of the SQLite database or in the agent's memory.
	SubCategoryFK                   *string `db:"sub_category_fk"`    // The subcategory foreign key. Can be found in the "categories" table of the SQLite database or in the agent's memory.
	WalletFK                        *string `db:"wallet_fk"`          // The wallet foreign key from the "wallets" table of the SQLite database.
	DateCreated                     int64   `db:"date_created"`       // The date the transaction was completed (timestamp format without seconds).
	DateTimeModified                int64   `db:"date_time_modified"` // The date the agent handled the transaction (timestamp format without seconds).
	OriginalDateDue                 *int64  `db:"original_date_due"`  // The date from the similar transaction in the SQLite database.
	Income                          *int64  `db:"income"`             // Actually a boolean value (can be 0 or 1).
	PeriodLength                    *int64  `db:"period_length"`      // The period length in different units (e.g., days, weeks, months). The unit depends on the reoccurrence key.
	Reoccurrence                    *int64  `db:"reoccurrence"`       // The reoccurrence key. Must be obtained from the SQLite database.
	EndDate                         *int64  `db:"end_date"`
	UpcomingTransactionNotification *int64  `db:"upcoming_transaction_notification"`
	Type                            *int64  `db:"type"`
	Paid                            *int64  `db:"paid"` // Always 1 if the transaction was completed. Can be 0 if it's a future transaction.
	CreatedAnotherFutureTransaction *int64  `db:"created_another_future_transaction"`
	SkipPaid                        *int64  `db:"skip_paid"`
	MethodAdded                     *int64  `db:"method_added"`
	TransactionOwnerEmail           *string `db:"transaction_owner_email"`
	TransactionOriginalOwnerEmail   *string `db:"transaction_original_owner_email"`
	SharedKey                       *string `db:"shared_key"`
	SharedOldKey                    *string `db:"shared_old_key"`
	SharedStatus                    *int64  `db:"shared_status"`
	SharedDateUpdated               *int64  `db:"shared_date_updated"`
	SharedReferenceBudgetPK         *string `db:"shared_reference_budget_pk"`
	ObjectiveFK                     *string `db:"objective_fk"`
	ObjectiveLoanFK                 *string `db:"objective_loan_fk"`
	BudgetFksExclude                *string `db:"budget_fks_exclude"`
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
	// Kind classifies Question so callers know how to route the user's reply.
	Kind QuestionKind
}

// QuestionKind classifies a question the Enricher asks the user.
type QuestionKind string

const (
	// QuestionWallet asks which wallet an unknown bank account belongs to.
	QuestionWallet QuestionKind = "wallet"
	// QuestionName asks for the name (then category) of an unknown merchant.
	QuestionName QuestionKind = "name"
	// QuestionClarify is informational: the item cannot be processed and no answer is expected.
	QuestionClarify QuestionKind = "clarify"
)
