package memory

// WalletMappings maps a bank account/card identifier (as printed in the SMS) and its currency to
// the Cashew wallet it corresponds to.
type WalletMappings struct {
	AccountPattern string `db:"account_pattern"`
	Name           string `db:"name"`
	Currency       string `db:"currency"`
	Type           string `db:"type"`
	WalletFK       string `db:"wallet_fk"`
}

// TransactionMappings is a learned mapping from a raw SMS transaction pattern (merchant/
// counterparty name) to the enriched transaction fields the agent should reuse for future
// transactions with a similar pattern.
//
// PairedTransactionFK, PeriodLength, Reoccurrence, EndDate, UpcomingTransactionNotification,
// Type, Paid, CreatedAnotherFutureTransaction, SkipPaid, ObjectiveFK and ObjectiveLoanFK have no
// source in the raw SMS data and are never supplied by the user: they must be resolved by copying
// them from a similar existing sqlite transaction, and left nil when no such transaction exists.
type TransactionMappings struct {
	TransactionPattern              string  `db:"transaction_pattern"`
	PairedTransactionFK             *string `db:"paired_transaction_fk"`
	Name                            *string `db:"name"`
	Note                            *string `db:"note"`
	CategoryFK                      *string `db:"category_fk"`
	SubCategoryFK                   *string `db:"sub_category_fk"`
	Income                          *int64  `db:"income"`
	PeriodLength                    *int64  `db:"period_length"`
	Reoccurrence                    *int64  `db:"reoccurrence"`
	EndDate                         *int64  `db:"end_date"`
	UpcomingTransactionNotification *int64  `db:"upcoming_transaction_notification"`
	Type                            *int64  `db:"type"`
	Paid                            *int64  `db:"paid"`
	CreatedAnotherFutureTransaction *int64  `db:"created_another_future_transaction"`
	SkipPaid                        *int64  `db:"skip_paid"`
	ObjectiveFK                     *string `db:"objective_fk"`
	ObjectiveLoanFK                 *string `db:"objective_loan_fk"`
}

// Candidate is a TransactionMappings returned by a similarity search, together with the trigram
// similarity score (0..1) against the queried pattern.
type Candidate struct {
	TransactionMappings
	Score float64
}
