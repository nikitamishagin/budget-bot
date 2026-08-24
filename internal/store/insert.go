package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/nikitamishagin/budget-bot/internal/handler"
)

// InsertResult reports the outcome of Store.Insert.
type InsertResult struct {
	// TransactionPK is the primary key of the inserted row, or of the matching pre-existing
	// transaction when Duplicate is true.
	TransactionPK string
	// Duplicate is true when a matching transaction already existed in the database and nothing
	// was inserted.
	Duplicate bool
}

// Insert adds exp as a new row in the "transactions" table. Before inserting, it checks whether a
// transaction with the same wallet, amount, name and date (same calendar day) already exists; if
// so, it reports the duplicate and inserts nothing, per AGENTS.md's "Duplicate/conflict handling"
// responsibility ("detect duplicate entries [...] and resolve conflicts").
func (s *Store) Insert(ctx context.Context, exp handler.EnrichedExpense) (InsertResult, error) {
	// Cashew stores all timestamp columns as Unix seconds (not milliseconds), per
	// llama-issue.md "Data format in the sqlite database" - verified against a real exported
	// Cashew database, whose date_created values are 10-digit second-epoch timestamps.
	dateSeconds := parseExpenseDate(exp.Date)

	// signedAmount stores the amount as Cashew does: negative for expenses, positive for income,
	// per llama-issue.md "Data format in the sqlite database".
	signedAmount := exp.Amount
	if !exp.Income {
		signedAmount = -signedAmount
	}

	// Per llama-issue.md "Not informative logs": log the JSON data of the expense being inserted,
	// so a failed/duplicate/successful write can always be traced back to what was attempted.
	if expJSON, err := json.Marshal(exp); err == nil {
		log.Info().RawJSON("expense", expJSON).Msg("inserting expense into database")
	} else {
		log.Info().Interface("expense", exp).Msg("inserting expense into database")
	}

	if pk, ok, err := s.findDuplicate(ctx, exp, signedAmount, dateSeconds); err != nil {
		return InsertResult{}, err
	} else if ok {
		log.Info().Str("transaction_pk", pk).Msg("duplicate expense detected, skipping insert")
		return InsertResult{TransactionPK: pk, Duplicate: true}, nil
	}

	pk := uuid.NewString()
	now := time.Now().Unix()

	var subCategory sql.NullString
	if exp.SubCategoryFK != "" {
		subCategory = sql.NullString{String: exp.SubCategoryFK, Valid: true}
	}

	// note stores the raw SMS description (exp.RawTopic), per llama-issue.md "Using raw
	// description from sms data": it must be kept in the database as the transaction's note, both
	// for user reference and so future lookups (FindExactMatch/FindCandidates) can match on it.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transactions (
			transaction_pk, name, amount, note, category_fk, sub_category_fk, wallet_fk,
			date_created, date_time_modified, original_date_due, income, paid, skip_paid
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0)`,
		pk, exp.Name, signedAmount, exp.RawTopic, exp.CategoryFK, subCategory, exp.WalletFK,
		dateSeconds, now, dateSeconds, boolToInt(exp.Income),
	)
	if err != nil {
		log.Error().Err(err).Str("transaction_pk", pk).Msg("failed to insert expense")
		return InsertResult{}, fmt.Errorf("insert transaction: %w", err)
	}
	log.Info().Str("transaction_pk", pk).Msg("expense inserted successfully")

	return InsertResult{TransactionPK: pk}, nil
}

// findDuplicate looks for an existing transaction with the same wallet, amount, name (case-insensitive)
// and date (same calendar day) as exp.
func (s *Store) findDuplicate(ctx context.Context, exp handler.EnrichedExpense, signedAmount float64, dateSeconds int64) (string, bool, error) {
	dayStart, dayEnd := dayBounds(dateSeconds)

	row := s.db.QueryRowContext(ctx, `
		SELECT transaction_pk FROM transactions
		WHERE wallet_fk = ? AND amount = ? AND name = ? COLLATE NOCASE
		  AND date_created >= ? AND date_created < ?
		LIMIT 1`, exp.WalletFK, signedAmount, exp.Name, dayStart, dayEnd)

	var pk string
	if err := row.Scan(&pk); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("check duplicate: %w", err)
	}
	return pk, true, nil
}

// dayBounds returns the [start, end) second-epoch bounds of the UTC calendar day containing
// seconds.
func dayBounds(seconds int64) (start, end int64) {
	t := time.Unix(seconds, 0).UTC()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart.Unix(), dayStart.AddDate(0, 0, 1).Unix()
}

// parseExpenseDate parses a llmparser.Expense.Date (RFC3339 "if determinable, otherwise
// as-is") into a second epoch timestamp (Cashew's convention, see llama-issue.md "Data format
// in the sqlite database"), falling back to now when it cannot be parsed.
func parseExpenseDate(date string) int64 {
	if t, err := time.Parse(time.RFC3339, date); err == nil {
		return t.Unix()
	}
	return time.Now().Unix()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
