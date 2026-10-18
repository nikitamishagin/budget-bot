package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nikitamishagin/budget-bot/tests/integrated/e2e"
)

func TestLoadExample(t *testing.T) {
	s, err := e2e.Load(filepath.Join("testdata", "example"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "example" || len(s.Database) == 0 {
		t.Fatalf("unexpected scenario: %+v", s.Name)
	}
	if len(s.Screenshots) != 2 || s.Screenshots[0].Name != "01-first.jpg" || s.Screenshots[1].MIME != "image/png" {
		t.Fatalf("unexpected screenshots: %+v", s.Screenshots)
	}
	if len(s.Expected.Transactions) != 1 || s.Expected.Transactions[0].Amount != -3.5 || s.Expected.Draft {
		t.Fatalf("unexpected expected: %+v", s.Expected)
	}
}

func TestLoadAll(t *testing.T) {
	all, err := e2e.LoadAll("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "example" {
		t.Fatalf("unexpected scenarios: %d", len(all))
	}
}

func TestLoadMissingParts(t *testing.T) {
	dir := t.TempDir()
	if _, err := e2e.Load(dir); err == nil {
		t.Fatal("expected error for empty folder")
	}
	if err := os.WriteFile(filepath.Join(dir, e2e.DatabaseFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e2e.Load(dir); err == nil {
		t.Fatal("expected error for missing screenshots")
	}
}
