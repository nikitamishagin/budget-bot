package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Candidate is a distinct (name, category, wallet, income) combination already present in the
// transactions table of the user's SQLite file, used as a fallback source of enrichment
// candidates when memory (internal/memory) has no match.
type Candidate struct {
	Name          string
	CategoryFK    string
	SubCategoryFK string
	WalletFK      string
	Income        bool
	// Count is how many existing transactions share this combination, used to rank candidates.
	Count int
}

// FindExactMatch returns the most common enrichment used for transactions whose raw SMS
// description (note) matches description exactly (case-insensitive), if any.
func (s *Store) FindExactMatch(ctx context.Context, description string) (Candidate, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, category_fk, COALESCE(sub_category_fk, ''), wallet_fk, income, COUNT(*) AS c
		FROM transactions
		WHERE note = ? COLLATE NOCASE
		GROUP BY name, category_fk, sub_category_fk, wallet_fk, income
		ORDER BY c DESC
		LIMIT 1`, description)

	c, err := scanCandidate(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Candidate{}, false, nil
		}
		return Candidate{}, false, fmt.Errorf("find exact match: %w", err)
	}
	return c, true, nil
}

func searchTokens(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	var tokens []string
	for _, f := range fields {
		if len(f) >= 3 {
			tokens = append(tokens, f)
		}
	}
	return tokens
}

// FindCandidates returns up to limit distinct enrichment combinations for transactions whose raw
// SMS description (note) contains description as a case-insensitive substring or matches tokens,
// most frequently used first. It never returns more than limit rows, so the SQLite file's contents
// are never dumped wholesale to the caller (e.g., an LLM prompt).
func (s *Store) FindCandidates(ctx context.Context, description string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 5
	}

	trimmed := strings.TrimSpace(description)
	whereClauses := []string{
		"note = ? COLLATE NOCASE",
		"(note != '' AND (note LIKE '%' || ? || '%' COLLATE NOCASE OR ? LIKE '%' || note || '%' COLLATE NOCASE))",
	}
	args := []any{trimmed, trimmed, trimmed}

	for _, token := range searchTokens(trimmed) {
		whereClauses = append(whereClauses, "note LIKE '%' || ? || '%' COLLATE NOCASE")
		args = append(args, token)
	}

	query := fmt.Sprintf(`
		SELECT name, category_fk, COALESCE(sub_category_fk, ''), wallet_fk, income, COUNT(*) AS c
		FROM transactions
		WHERE %s
		GROUP BY name, category_fk, sub_category_fk, wallet_fk, income
		ORDER BY c DESC
		LIMIT ?`, strings.Join(whereClauses, " OR "))

	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCandidate(row rowScanner) (Candidate, error) {
	var c Candidate
	var income int
	if err := row.Scan(&c.Name, &c.CategoryFK, &c.SubCategoryFK, &c.WalletFK, &income, &c.Count); err != nil {
		return Candidate{}, err
	}
	c.Income = income != 0
	return c, nil
}

// SimilarTransaction holds the enrichment fields that have no source in raw SMS data and are
// never supplied by the user (see AGENTS.md's field resolution rule): the agent must copy them
// from a similar existing transaction rather than invent defaults or ask the user.
type SimilarTransaction struct {
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

// FindSimilarTransaction returns the no-source enrichment fields of the most recent existing
// transaction whose name or raw SMS description (note) matches description, so the agent
// can copy them instead of inventing defaults. ok is false when no such transaction exists.
func (s *Store) FindSimilarTransaction(ctx context.Context, description string) (SimilarTransaction, bool, error) {
	trimmed := strings.TrimSpace(description)
	whereClauses := []string{
		"name = ? COLLATE NOCASE",
		"note = ? COLLATE NOCASE",
		"(note != '' AND (note LIKE '%' || ? || '%' COLLATE NOCASE OR ? LIKE '%' || note || '%' COLLATE NOCASE))",
	}
	args := []any{trimmed, trimmed, trimmed, trimmed}

	for _, token := range searchTokens(trimmed) {
		whereClauses = append(whereClauses, "note LIKE '%' || ? || '%' COLLATE NOCASE", "name LIKE '%' || ? || '%' COLLATE NOCASE")
		args = append(args, token, token)
	}

	query := fmt.Sprintf(`
		SELECT NULLIF(original_date_due, ''), NULLIF(paired_transaction_fk, ''), NULLIF(period_length, ''),
		       NULLIF(reoccurrence, ''), NULLIF(end_date, ''),
		       NULLIF(upcoming_transaction_notification, ''), NULLIF(type, ''), NULLIF(paid, ''),
		       NULLIF(created_another_future_transaction, ''), NULLIF(skip_paid, ''),
		       NULLIF(objective_fk, ''), NULLIF(objective_loan_fk, '')
		FROM transactions
		WHERE %s
		ORDER BY date_created DESC
		LIMIT 1`, strings.Join(whereClauses, " OR "))

	row := s.db.QueryRowContext(ctx, query, args...)

	var (
		originalDateDue, periodLength, reoccurrence, endDate       sql.NullInt64
		notification, txType, paid, createdAnotherFuture, skipPaid sql.NullInt64
		pairedTransactionFK, objectiveFK, objectiveLoanFK          sql.NullString
	)
	if err := row.Scan(
		&originalDateDue, &pairedTransactionFK, &periodLength, &reoccurrence, &endDate,
		&notification, &txType, &paid, &createdAnotherFuture,
		&skipPaid, &objectiveFK, &objectiveLoanFK,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SimilarTransaction{}, false, nil
		}
		return SimilarTransaction{}, false, fmt.Errorf("find similar transaction: %w", err)
	}

	return SimilarTransaction{
		OriginalDateDue:                 nullInt64Ptr(originalDateDue),
		PairedTransactionFK:             nullStringPtr(pairedTransactionFK),
		PeriodLength:                    nullInt64Ptr(periodLength),
		Reoccurrence:                    nullInt64Ptr(reoccurrence),
		EndDate:                         nullInt64Ptr(endDate),
		UpcomingTransactionNotification: nullInt64Ptr(notification),
		Type:                            nullInt64Ptr(txType),
		Paid:                            nullInt64Ptr(paid),
		CreatedAnotherFutureTransaction: nullInt64Ptr(createdAnotherFuture),
		SkipPaid:                        nullInt64Ptr(skipPaid),
		ObjectiveFK:                     nullStringPtr(objectiveFK),
		ObjectiveLoanFK:                 nullStringPtr(objectiveLoanFK),
	}, true, nil
}

// nullInt64Ptr converts a sql.NullInt64 into a *int64, nil when the column was NULL.
func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// nullStringPtr converts a sql.NullString into a *string, nil when the column was NULL.
func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}
