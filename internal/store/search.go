package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// description (note) or display name matches topic exactly (case-insensitive), if any. Matching
// on note lets the raw description drive lookups, while still falling back to name for
// transactions inserted before note was tracked.
func (s *Store) FindExactMatch(ctx context.Context, topic string) (Candidate, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, category_fk, COALESCE(sub_category_fk, ''), wallet_fk, income, COUNT(*) AS c
		FROM transactions
		WHERE note = ? COLLATE NOCASE OR name = ? COLLATE NOCASE
		GROUP BY name, category_fk, sub_category_fk, wallet_fk, income
		ORDER BY c DESC
		LIMIT 1`, topic, topic)

	c, err := scanCandidate(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Candidate{}, false, nil
		}
		return Candidate{}, false, fmt.Errorf("find exact match: %w", err)
	}
	return c, true, nil
}

// FindCandidates returns up to limit distinct enrichment combinations for transactions whose raw
// SMS description (note) or display name contains topic as a case-insensitive substring, most
// frequently used first. It never returns more than limit rows, so the SQLite file's contents are
// never dumped wholesale to the caller (e.g., an LLM prompt).
func (s *Store) FindCandidates(ctx context.Context, topic string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT name, category_fk, COALESCE(sub_category_fk, ''), wallet_fk, income, COUNT(*) AS c
		FROM transactions
		WHERE note LIKE '%' || ? || '%' COLLATE NOCASE OR name LIKE '%' || ? || '%' COLLATE NOCASE
		GROUP BY name, category_fk, sub_category_fk, wallet_fk, income
		ORDER BY c DESC
		LIMIT ?`, topic, topic, limit)
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
