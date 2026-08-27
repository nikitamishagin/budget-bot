// Package store gives the agent read/write access to a user's uploaded Cashew SQLite database
// file: finding candidate categories/wallets already used for a similar transaction name, and
// inserting new transactions with duplicate detection.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// requiredTables are the Cashew tables the agent relies on; a file missing any of them is not
// treated as a valid Cashew database.
var requiredTables = []string{"transactions", "categories", "wallets"}

// Store wraps a single Cashew SQLite database file.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database file at the path and verifies that it looks like a Cashew database
// before returning it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite file: %w", err)
	}
	// The sqlite driver only supports a single writer at a time; keep this simple and safe by
	// never opening more than one connection to the file.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite file: %w", err)
	}

	if err := verifySchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func verifySchema(db *sql.DB) error {
	var count int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN (?, ?, ?)`,
		requiredTables[0], requiredTables[1], requiredTables[2],
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("verify cashew schema: %w", err)
	}
	if count != len(requiredTables) {
		return fmt.Errorf("file does not look like a Cashew database (missing one of %v)", requiredTables)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
