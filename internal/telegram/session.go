package telegram

import (
	"sync"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
)

// pendingKind identifies what kind of answer a chat is waiting for.
type pendingKind int

const (
	// pendingWallet means the bot is waiting for the user to name the wallet a bank account
	// belongs to.
	pendingWallet pendingKind = iota
	// pendingCategory means the bot is waiting for the user to name the category for a
	// transaction the LLM could not confidently resolve.
	pendingCategory
	// pendingApproval means the bot is waiting for the user to confirm ("yes"/"no") that an
	// already-enriched expense should be saved.
	pendingApproval
)

// pendingState tracks what a single chat is waiting for next.
type pendingState struct {
	kind     pendingKind
	expense  llmparser.Expense
	enriched *handler.EnrichedExpense
}

// sessions tracks per-chat pending state, guarded by a mutex since updates are processed
// concurrently by the Telegram update loop.
type sessions struct {
	mu    sync.Mutex
	state map[int64]*pendingState
}

func newSessions() *sessions {
	return &sessions{state: make(map[int64]*pendingState)}
}

func (s *sessions) get(chatID int64) (*pendingState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[chatID]
	return st, ok
}

func (s *sessions) set(chatID int64, st *pendingState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[chatID] = st
}

func (s *sessions) clear(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state, chatID)
}

// databases tracks, per chat, the local path of the SQLite database file the user uploaded.
// TELEGRAM_GUIDELINES.md requires the agent to receive this file before it will process any
// screenshots.
type databases struct {
	mu    sync.Mutex
	paths map[int64]string
}

func newDatabases() *databases {
	return &databases{paths: make(map[int64]string)}
}

func (d *databases) get(chatID int64) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.paths[chatID]
	return p, ok
}

func (d *databases) set(chatID int64, path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths[chatID] = path
}
