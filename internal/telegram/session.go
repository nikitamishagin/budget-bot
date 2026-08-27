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
	// pendingName means the bot is waiting for the user to name the expense itself, for a
	// transaction the LLM could not confidently resolve.
	pendingName
	// pendingCategory means the bot is waiting for the user to name the category for a
	// transaction the LLM could not confidently resolve.
	pendingCategory
	// pendingSubCategory means the bot is waiting for the user to name the (optional)
	// subcategory for a transaction the LLM could not confidently resolve.
	pendingSubCategory
	// pendingApproval means the bot is waiting for the user to confirm ("yes"/"no") that a whole
	// batch of already-enriched expenses (parsed from every queued screenshot) should be saved,
	// per AGENTS.md step 13.
	pendingApproval
	// pendingEditing means the bot rejected/discarded a batch (per AGENTS.md step 14) and is
	// waiting for the user to either fix individual entries, approve the batch as-is, or discard
	// it entirely.
	pendingEditing
)

// pendingState tracks what a single chat is waiting for next.
type pendingState struct {
	kind pendingKind
	// expense is the raw parsed expense currently being enriched (kind == pendingWallet,
	// pendingName, pendingCategory, or pendingSubCategory).
	expense llmparser.Expense
	// name/categoryFK accumulate the answers already given while walking through pendingName,
	// pendingCategory, and pendingSubCategory in sequence for the current expense.
	name       string
	categoryFK string
	// queue holds the remaining raw expenses (from every queued screenshot) still waiting to be
	// enriched, per AGENTS.md step 5 ("every image from the queue must be parsed one by one").
	queue []llmparser.Expense
	// batch accumulates every expense already enriched during the current /parse run, per
	// AGENTS.md step 6 ("aggregates answers from llm [...] to one list").
	batch []*handler.EnrichedExpense
}

// screenshots tracks, per chat, the raw image bytes waiting to be parsed once the user sends
// /parse, per AGENTS.md steps 3-5 ("received screenshots must be saved to a queue").
type screenshots struct {
	mu    sync.Mutex
	items map[int64][]screenshot
}

// screenshot is a single queued image, downloaded but not yet parsed.
type screenshot struct {
	data []byte
	mime string
}

func newScreenshots() *screenshots {
	return &screenshots{items: make(map[int64][]screenshot)}
}

func (s *screenshots) add(chatID int64, data []byte, mime string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[chatID] = append(s.items[chatID], screenshot{data: data, mime: mime})
	return len(s.items[chatID])
}

func (s *screenshots) take(chatID int64) []screenshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items[chatID]
	delete(s.items, chatID)
	return items
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
