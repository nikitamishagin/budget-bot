package e2e_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	if !strings.Contains(s.MemorySQL, "example scenario") {
		t.Fatalf("unexpected memory sql: %q", s.MemorySQL)
	}
}

func TestLoadAll(t *testing.T) {
	all, err := e2e.LoadAll("testdata")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range all {
		if s.Name == "example" {
			found = true
		}
	}
	if !found {
		t.Fatalf("example scenario not found among %d scenarios", len(all))
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

func TestLoadMissingMemory(t *testing.T) {
	src := filepath.Join("testdata", "example")
	dir := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if rel == e2e.MemoryFile {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2e.Load(dir); err == nil {
		t.Fatal("expected error for missing memory.sql")
	}
}
