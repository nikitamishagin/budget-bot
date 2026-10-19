// Package e2e holds the layout and loader for end-to-end scenarios that drive the whole agent
// through a fake Telegram user (see internal/telegram/telegramtest). The loader itself has no
// external dependencies, so it is covered by the default `go test ./...`; the scenarios that use
// it live behind the integration build tag.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// DatabaseFile is the starting Cashew SQLite file inside a scenario folder.
	DatabaseFile = "database.sqlite"
	// ScreenshotsDir holds the screenshots, sent in file name order.
	ScreenshotsDir = "screenshots"
	// ExpectedFile holds the expected results.
	ExpectedFile = "expected.json"
	// MemoryFile is the SQL script that seeds the agent memory for a scenario. It is required,
	// but may be empty.
	MemoryFile = "memory.sql"
)

// Screenshot is one screenshot to send to the bot.
type Screenshot struct {
	Name string
	MIME string
	Data []byte
}

// ExpectedTransaction is a transaction that must be present in the returned database. Category
// and wallet are names, not keys, so the file stays readable.
type ExpectedTransaction struct {
	Name     string  `json:"name"`
	Amount   float64 `json:"amount"`
	Category string  `json:"category,omitempty"`
	Wallet   string  `json:"wallet,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// Expected is the content of expected.json.
type Expected struct {
	Description string `json:"description,omitempty"`
	// Draft marks results generated from a real run; they may contain current bugs and must be
	// reviewed manually, then Draft set to false, before a scenario is trusted.
	Draft         bool                  `json:"draft"`
	Transactions  []ExpectedTransaction `json:"transactions"`
	ReplyContains []string              `json:"reply_contains,omitempty"`
}

// Scenario is one loaded scenario folder.
type Scenario struct {
	Name        string
	Dir         string
	Database    []byte
	Screenshots []Screenshot
	MemorySQL   string
	Expected    Expected
}

// Load reads one scenario folder.
func Load(dir string) (*Scenario, error) {
	db, err := os.ReadFile(filepath.Join(dir, DatabaseFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", DatabaseFile, err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, ScreenshotsDir))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ScreenshotsDir, err)
	}
	var shots []Screenshot
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		mime, ok := mimeByExt(entry.Name())
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ScreenshotsDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read screenshot %s: %w", entry.Name(), err)
		}
		shots = append(shots, Screenshot{Name: entry.Name(), MIME: mime, Data: data})
	}
	if len(shots) == 0 {
		return nil, fmt.Errorf("no screenshots found in %s", filepath.Join(dir, ScreenshotsDir))
	}
	sort.Slice(shots, func(i, j int) bool { return shots[i].Name < shots[j].Name })

	raw, err := os.ReadFile(filepath.Join(dir, ExpectedFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ExpectedFile, err)
	}
	var expected Expected
	if err := json.Unmarshal(raw, &expected); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ExpectedFile, err)
	}

	memory, err := os.ReadFile(filepath.Join(dir, MemoryFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", MemoryFile, err)
	}

	return &Scenario{
		Name:        filepath.Base(dir),
		Dir:         dir,
		Database:    db,
		Screenshots: shots,
		MemorySQL:   string(memory),
		Expected:    expected,
	}, nil
}

// LoadAll loads every scenario folder directly under root, in name order.
func LoadAll(root string) ([]*Scenario, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read scenarios root: %w", err)
	}
	var scenarios []*Scenario
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		s, err := Load(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("scenario %s: %w", entry.Name(), err)
		}
		scenarios = append(scenarios, s)
	}
	return scenarios, nil
}

func mimeByExt(name string) (string, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	}
	return "", false
}
