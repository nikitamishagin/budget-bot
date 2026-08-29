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
// so, it reports the duplicate and inserts nothing. exp is expected to already carry its final,
// enrichment-computed values (signed amount, date_created/date_time_modified as Unix seconds,
// etc.) - see internal/handler.Enricher, which builds it.
func (s *Store) Insert(ctx context.Context, exp handler.EnrichedExpense) (InsertResult, error) {
	// Log the JSON data of the expense being inserted, so a failed/duplicate/successful write can
	// always be traced back to what was attempted.
	if expJSON, err := json.Marshal(exp); err == nil {
		log.Info().RawJSON("expense", expJSON).Msg("inserting expense into database")
	} else {
		log.Info().Interface("expense", exp).Msg("inserting expense into database")
	}

	if pk, ok, err := s.findDuplicate(ctx, exp); err != nil {
		return InsertResult{}, err
	} else if ok {
		log.Info().Str("transaction_pk", pk).Msg("duplicate expense detected, skipping insert")
		return InsertResult{TransactionPK: pk, Duplicate: true}, nil
	}

	pk := uuid.NewString()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transactions (
			transaction_pk, paired_transaction_fk, name, amount, note, category_fk, sub_category_fk,
			wallet_fk, date_created, date_time_modified, original_date_due, income, period_length,
			reoccurrence, end_date, upcoming_transaction_notification, type, paid,
			created_another_future_transaction, skip_paid, objective_fk, objective_loan_fk
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pk, exp.PairedTransactionFK, strVal(exp.Name), exp.Amount, strVal(exp.Note), strVal(exp.CategoryFK),
		nullString(exp.SubCategoryFK), strVal(exp.WalletFK), exp.DateCreated, exp.DateTimeModified,
		exp.OriginalDateDue, int64Val(exp.Income), exp.PeriodLength, exp.Reoccurrence, exp.EndDate,
		exp.UpcomingTransactionNotification, exp.Type, int64Val(exp.Paid), exp.CreatedAnotherFutureTransaction,
		int64Val(exp.SkipPaid), exp.ObjectiveFK, exp.ObjectiveLoanFK,
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
func (s *Store) findDuplicate(ctx context.Context, exp handler.EnrichedExpense) (string, bool, error) {
	dayStart, dayEnd := dayBounds(exp.DateCreated)

	row := s.db.QueryRowContext(ctx, `
		SELECT transaction_pk FROM transactions
		WHERE wallet_fk = ? AND amount = ? AND name = ? COLLATE NOCASE
		  AND date_created >= ? AND date_created < ?
		LIMIT 1`, strVal(exp.WalletFK), exp.Amount, strVal(exp.Name), dayStart, dayEnd)

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

// strVal dereferences a possibly-nil string pointer, returning "" when nil.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// int64Val dereferences a possibly-nil int64 pointer, returning 0 when nil.
func int64Val(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// nullString converts a possibly-nil string pointer into a sql.NullString, so an unset
// sub_category_fk is stored as SQL NULL rather than an empty string.
func nullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}
