// Package memory implements the agent's Postgres-backed memory: learned mappings from raw SMS
// topics to enriched expense data, and static bank account -> wallet mappings. It provides
// exact/high-confidence matching (to skip the LLM entirely) and top-N fuzzy candidate search (to
// keep LLM context small), as described in MEMORY-GUIDELINES.md.
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

// FindBestMatch returns the training pair whose raw topic is the most similar to a topic, along
// with its trigram similarity score. ok is false when no training pair exists at all.
func (s *Store) FindBestMatch(ctx context.Context, topic string) (candidate Candidate, ok bool, err error) {
	row := s.pool.QueryRow(ctx, `
		SELECT raw_topic, name, category_fk, COALESCE(sub_category_fk, ''), wallet_fk, income,
		       similarity(raw_topic, $1) AS score
		FROM training_pairs
		ORDER BY score DESC, hit_count DESC
		LIMIT 1
	`, topic)

	var c Candidate
	if err := row.Scan(&c.RawDesc, &c.Name, &c.CategoryFK, &c.SubCategoryFK, &c.WalletFK, &c.Income, &c.Score); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Candidate{}, false, nil
		}
		return Candidate{}, false, fmt.Errorf("find best match: %w", err)
	}
	return c, true, nil
}

// TopCandidates returns up to n training pairs most similar to the topic, ordered by descending
// similarity score. Only candidates with a non-zero similarity are returned.
func (s *Store) TopCandidates(ctx context.Context, topic string, n int) ([]Candidate, error) {
	if n <= 0 {
		n = DefaultTopN
	}

	rows, err := s.pool.Query(ctx, `
		SELECT raw_topic, name, category_fk, COALESCE(sub_category_fk, ''), wallet_fk, income,
		       similarity(raw_topic, $1) AS score
		FROM training_pairs
		WHERE similarity(raw_topic, $1) > 0
		ORDER BY score DESC, hit_count DESC
		LIMIT $2
	`, topic, n)
	if err != nil {
		return nil, fmt.Errorf("query top candidates: %w", err)
	}
	defer rows.Close()

	var candidates []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.RawDesc, &c.Name, &c.CategoryFK, &c.SubCategoryFK, &c.WalletFK, &c.Income, &c.Score); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return candidates, nil
}

// SaveTrainingPair inserts or reinforces a learned topic -> enrichment mapping. If a pair with
// the same raw topic, category, and wallet already exists, its hit_count is incremented instead of
// creating a duplicate row.
func (s *Store) SaveTrainingPair(ctx context.Context, pair TrainingPair) error {
	topic := strings.TrimSpace(pair.RawDesc)
	if topic == "" {
		return fmt.Errorf("raw topic must not be empty")
	}
	if pair.CategoryFK == "" {
		return fmt.Errorf("category_fk must not be empty")
	}
	if pair.WalletFK == "" {
		return fmt.Errorf("wallet_fk must not be empty")
	}

	var subCategory *string
	if pair.SubCategoryFK != "" {
		subCategory = &pair.SubCategoryFK
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO training_pairs (raw_topic, name, category_fk, sub_category_fk, wallet_fk, income)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (raw_topic, category_fk, wallet_fk) DO UPDATE
		SET hit_count       = training_pairs.hit_count + 1,
		    name            = EXCLUDED.name,
		    sub_category_fk = EXCLUDED.sub_category_fk,
		    income          = EXCLUDED.income,
		    updated_at      = now()
	`, topic, pair.Name, pair.CategoryFK, subCategory, pair.WalletFK, pair.Income)
	if err != nil {
		return fmt.Errorf("save training pair: %w", err)
	}
	return nil
}

// ResolveAccount looks up the wallet a bank account/card identifier and currency combination is
// mapped to. Mappings are keyed by (account, currency) since a single card can produce expenses
// in several currencies that must map to different wallets, per llama-issue.md "Wallet matching".
// ok is false when no mapping exists.
func (s *Store) ResolveAccount(ctx context.Context, accountPattern, currency string) (walletFK string, ok bool, err error) {
	row := s.pool.QueryRow(ctx, `
		SELECT wallet_fk FROM bank_account_mappings WHERE account_pattern = $1 AND currency = $2
	`, accountPattern, currency)

	if err := row.Scan(&walletFK); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve account: %w", err)
	}
	return walletFK, true, nil
}

// SaveAccountMapping inserts or updates the wallet an (account, currency) combination maps to.
func (s *Store) SaveAccountMapping(ctx context.Context, accountPattern, currency, walletFK string) error {
	if strings.TrimSpace(accountPattern) == "" {
		return fmt.Errorf("account pattern must not be empty")
	}
	if walletFK == "" {
		return fmt.Errorf("wallet_fk must not be empty")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO bank_account_mappings (account_pattern, currency, wallet_fk)
		VALUES ($1, $2, $3)
		ON CONFLICT (account_pattern, currency) DO UPDATE
		SET wallet_fk  = EXCLUDED.wallet_fk,
		    updated_at = now()
	`, accountPattern, currency, walletFK)
	if err != nil {
		return fmt.Errorf("save account mapping: %w", err)
	}
	return nil
}
