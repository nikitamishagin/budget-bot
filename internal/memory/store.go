// Package memory implements the agent's Postgres-backed memory: learned mappings from raw SMS
// transaction patterns to enriched transaction data, and static bank account -> wallet mappings.
// It provides exact/high-confidence matching (to skip the LLM entirely) and top-N fuzzy candidate
// search (to keep LLM context small).
package memory

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// HighConfidenceThreshold is the trigram similarity score above which a match is considered
// reliable enough to skip the LLM entirely.
const HighConfidenceThreshold = 0.9

// DefaultTopN is the default number of similar candidates returned by TopCandidates.
const DefaultTopN = 5

// Store is a Postgres-backed memory store.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres using the given DSN (e.g.
// "postgres://user:pass@host:5432/dbname?sslmode=disable") and returns a Store.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Migrate creates the memory schema (tables, extension, indexes) if it does not already exist.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply memory schema: %w", err)
	}
	return nil
}

// FindBestMatch returns the transaction mapping whose pattern is the most similar to a pattern,
// along with its trigram similarity score. ok is false when no mapping exists at all.
func (s *Store) FindBestMatch(ctx context.Context, pattern string) (candidate Candidate, ok bool, err error) {
	row := s.pool.QueryRow(ctx, `
		SELECT transaction_pattern, paired_transaction_fk, name, note, category_fk, sub_category_fk,
		       income, period_length, reoccurrence, end_date, upcoming_transaction_notification,
		       type, paid, created_another_future_transaction, skip_paid, objective_fk, objective_loan_fk,
		       similarity(transaction_pattern, $1) AS score
		FROM transaction_mappings
		ORDER BY score DESC, hit_count DESC
		LIMIT 1
	`, pattern)

	var c Candidate
	if err := scanTransactionMapping(row, &c.TransactionMappings, &c.Score); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Candidate{}, false, nil
		}
		return Candidate{}, false, fmt.Errorf("find best match: %w", err)
	}
	return c, true, nil
}

// TopCandidates returns up to n transaction mappings most similar to the pattern, ordered by
// descending similarity score. Only candidates with a non-zero similarity are returned.
func (s *Store) TopCandidates(ctx context.Context, pattern string, n int) ([]Candidate, error) {
	if n <= 0 {
		n = DefaultTopN
	}

	rows, err := s.pool.Query(ctx, `
		SELECT transaction_pattern, paired_transaction_fk, name, note, category_fk, sub_category_fk,
		       income, period_length, reoccurrence, end_date, upcoming_transaction_notification,
		       type, paid, created_another_future_transaction, skip_paid, objective_fk, objective_loan_fk,
		       similarity(transaction_pattern, $1) AS score
		FROM transaction_mappings
		WHERE similarity(transaction_pattern, $1) > 0
		ORDER BY score DESC, hit_count DESC
		LIMIT $2
	`, pattern, n)
	if err != nil {
		return nil, fmt.Errorf("query top candidates: %w", err)
	}
	defer rows.Close()

	var candidates []Candidate
	for rows.Next() {
		var c Candidate
		if err := scanTransactionMapping(rows, &c.TransactionMappings, &c.Score); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return candidates, nil
}

// scanRow is the subset of pgx.Row/pgx.Rows needed to scan a transaction_mappings row.
type scanRow interface {
	Scan(dest ...any) error
}

// scanTransactionMapping scans one transaction_mappings row (as selected by FindBestMatch and
// TopCandidates, in that exact column order) into m and score.
func scanTransactionMapping(row scanRow, m *TransactionMappings, score *float64) error {
	return row.Scan(
		&m.TransactionPattern, &m.PairedTransactionFK, &m.Name, &m.Note, &m.CategoryFK, &m.SubCategoryFK,
		&m.Income, &m.PeriodLength, &m.Reoccurrence, &m.EndDate, &m.UpcomingTransactionNotification,
		&m.Type, &m.Paid, &m.CreatedAnotherFutureTransaction, &m.SkipPaid, &m.ObjectiveFK, &m.ObjectiveLoanFK,
		score,
	)
}

// SaveTransactionMapping inserts or reinforces a learned pattern -> enrichment mapping. If a
// mapping with the same transaction pattern already exists, it is updated in place and its
// hit_count is incremented, rather than creating a duplicate row.
func (s *Store) SaveTransactionMapping(ctx context.Context, m TransactionMappings) error {
	pattern := strings.TrimSpace(m.TransactionPattern)
	if pattern == "" {
		return fmt.Errorf("transaction pattern must not be empty")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO transaction_mappings (
			transaction_pattern, paired_transaction_fk, name, note, category_fk, sub_category_fk,
			income, period_length, reoccurrence, end_date, upcoming_transaction_notification,
			type, paid, created_another_future_transaction, skip_paid, objective_fk, objective_loan_fk
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (transaction_pattern) DO UPDATE
		SET hit_count                          = transaction_mappings.hit_count + 1,
		    paired_transaction_fk              = EXCLUDED.paired_transaction_fk,
		    name                               = EXCLUDED.name,
		    note                               = EXCLUDED.note,
		    category_fk                        = EXCLUDED.category_fk,
		    sub_category_fk                    = EXCLUDED.sub_category_fk,
		    income                             = EXCLUDED.income,
		    period_length                      = EXCLUDED.period_length,
		    reoccurrence                       = EXCLUDED.reoccurrence,
		    end_date                           = EXCLUDED.end_date,
		    upcoming_transaction_notification  = EXCLUDED.upcoming_transaction_notification,
		    type                               = EXCLUDED.type,
		    paid                               = EXCLUDED.paid,
		    created_another_future_transaction = EXCLUDED.created_another_future_transaction,
		    skip_paid                          = EXCLUDED.skip_paid,
		    objective_fk                       = EXCLUDED.objective_fk,
		    objective_loan_fk                  = EXCLUDED.objective_loan_fk,
		    updated_at                         = now()
	`, pattern, m.PairedTransactionFK, m.Name, m.Note, m.CategoryFK, m.SubCategoryFK,
		m.Income, m.PeriodLength, m.Reoccurrence, m.EndDate, m.UpcomingTransactionNotification,
		m.Type, m.Paid, m.CreatedAnotherFutureTransaction, m.SkipPaid, m.ObjectiveFK, m.ObjectiveLoanFK)
	if err != nil {
		return fmt.Errorf("save transaction mapping: %w", err)
	}
	return nil
}

// ResolveWallet looks up the wallet a bank account/card identifier and currency combination is
// mapped to. Mappings are keyed by (account, currency) since a single card can produce expenses
// in several currencies that must map to different wallets. ok is false when no mapping exists.
func (s *Store) ResolveWallet(ctx context.Context, accountPattern, currency string) (WalletMappings, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT account_pattern, currency, name, type, wallet_fk
		FROM wallet_mappings WHERE account_pattern = $1 AND currency = $2
	`, accountPattern, currency)

	var w WalletMappings
	if err := row.Scan(&w.AccountPattern, &w.Currency, &w.Name, &w.Type, &w.WalletFK); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WalletMappings{}, false, nil
		}
		return WalletMappings{}, false, fmt.Errorf("resolve wallet: %w", err)
	}
	return w, true, nil
}

// SaveWalletMapping inserts or updates the wallet an (account, currency) combination maps to.
func (s *Store) SaveWalletMapping(ctx context.Context, m WalletMappings) error {
	if strings.TrimSpace(m.AccountPattern) == "" {
		return fmt.Errorf("account pattern must not be empty")
	}
	if m.WalletFK == "" {
		return fmt.Errorf("wallet_fk must not be empty")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO wallet_mappings (account_pattern, currency, name, type, wallet_fk)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (account_pattern, currency) DO UPDATE
		SET name       = EXCLUDED.name,
		    type       = EXCLUDED.type,
		    wallet_fk  = EXCLUDED.wallet_fk,
		    updated_at = now()
	`, m.AccountPattern, m.Currency, m.Name, m.Type, m.WalletFK)
	if err != nil {
		return fmt.Errorf("save wallet mapping: %w", err)
	}
	return nil
}
