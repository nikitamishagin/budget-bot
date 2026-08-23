package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

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
	dateMillis := parseExpenseDate(exp.Date)

	if pk, ok, err := s.findDuplicate(ctx, exp, dateMillis); err != nil {
		return InsertResult{}, err
	} else if ok {
		return InsertResult{TransactionPK: pk, Duplicate: true}, nil
	}

	pk := uuid.NewString()
	now := time.Now().UnixMilli()

	var subCategory sql.NullString
	if exp.SubCategoryFK != "" {
		subCategory = sql.NullString{String: exp.SubCategoryFK, Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transactions (
			transaction_pk, name, amount, note, category_fk, sub_category_fk, wallet_fk,
			date_created, date_time_modified, original_date_due, income, paid, skip_paid
		) VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, 1, 0)`,
		pk, exp.Name, exp.Amount, exp.CategoryFK, subCategory, exp.WalletFK,
		dateMillis, now, dateMillis, boolToInt(exp.Income),
	)
	if err != nil {
		return InsertResult{}, fmt.Errorf("insert transaction: %w", err)
	}

	return InsertResult{TransactionPK: pk}, nil
}

// findDuplicate looks for an existing transaction with the same wallet, amount, name (case-insensitive)
// and date (same calendar day) as exp.
func (s *Store) findDuplicate(ctx context.Context, exp handler.EnrichedExpense, dateMillis int64) (string, bool, error) {
	dayStart, dayEnd := dayBounds(dateMillis)

	row := s.db.QueryRowContext(ctx, `
		SELECT transaction_pk FROM transactions
		WHERE wallet_fk = ? AND amount = ? AND name = ? COLLATE NOCASE
		  AND date_created >= ? AND date_created < ?
		LIMIT 1`, exp.WalletFK, exp.Amount, exp.Name, dayStart, dayEnd)

	var pk string
	if err := row.Scan(&pk); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("check duplicate: %w", err)
	}
	return pk, true, nil
}

// dayBounds returns the [start, end) millisecond-epoch bounds of the UTC calendar day
// containing millis.
func dayBounds(millis int64) (start, end int64) {
	t := time.UnixMilli(millis).UTC()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart.UnixMilli(), dayStart.AddDate(0, 0, 1).UnixMilli()
}

// parseExpenseDate parses a llmparser.Expense.Date (RFC3339 "if determinable, otherwise
// as-is") into a millisecond epoch timestamp, falling back to now when it cannot be parsed.
func parseExpenseDate(date string) int64 {
	if t, err := time.Parse(time.RFC3339, date); err == nil {
		return t.UnixMilli()
	}
	return time.Now().UnixMilli()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
