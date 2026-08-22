package llmparser

// TransactionType classifies the kind of transaction found in the SMS.
type TransactionType string

const (
	// TransactionExpense is a regular purchase/payment.
	TransactionExpense TransactionType = "expense"
	// TransactionRefund is money returned to the account.
	TransactionRefund TransactionType = "refund"
	// TransactionTransfer is a transfer between the user's own accounts.
	TransactionTransfer TransactionType = "transfer"
	// TransactionIncome is incoming money that is not a refund or a transfer.
	TransactionIncome TransactionType = "income"
	// TransactionUnknown marks an SMS that could not be classified or parsed.
	TransactionUnknown TransactionType = "unknown"
)

// Expense is the structured result of parsing a single bank SMS screenshot.
type Expense struct {
	// Type classifies the transaction, see TransactionType constants.
	Type TransactionType `json:"type"`
	// Amount is the transaction amount as printed in the SMS (always positive).
	Amount float64 `json:"amount"`
	// Currency is the ISO 4217 currency code, e.g. "USD", "RUB".
	Currency string `json:"currency"`
	// Account is the bank account/card identifier mentioned in the SMS (e.g. last 4 digits).
	Account string `json:"bank_account"`
	// Date is the transaction date/time as printed in the SMS, in RFC3339 if determinable, otherwise as-is.
	Date string `json:"date"`
	// Topic is the merchant/counterparty name, or the other account for transfers.
	Topic string `json:"topic"`
	// ParseFailed indicates the model could not confidently extract data from the image.
	ParseFailed bool `json:"parse_failed"`
	// FailureReason explains why parsing failed, empty when ParseFailed is false.
	FailureReason string `json:"failure_reason,omitempty"`
}
