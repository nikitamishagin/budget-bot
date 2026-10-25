package handler

import (
	"strings"
	"time"
)

// expenseDateLayouts lists the non-RFC3339 date layouts accepted by parseExpenseDate,
// tried in order.
var expenseDateLayouts = []string{
	"2006.01.02 15:04",
	"2006.01.02 15:04:05",
	"2006.01.02",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
	"02.01.2006 15:04",
	"02.01.2006",
}

// parseExpenseDate parses a llmparser.Expense.Date into a second-epoch timestamp truncated to
// the minute. RFC3339 values keep their own offset; other known layouts are interpreted in loc
// (time.Local when nil). The second return value is false when the date is empty or unparseable.
func parseExpenseDate(date string, loc *time.Location) (int64, bool) {
	s := strings.TrimSpace(date)
	if s == "" {
		return 0, false
	}
	if loc == nil {
		loc = time.Local
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Truncate(time.Minute).Unix(), true
	}
	for _, layout := range expenseDateLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.Truncate(time.Minute).Unix(), true
		}
	}
	return 0, false
}

// ParseExpenseDate is the exported form of parseExpenseDate, so other packages (e.g. the
// Telegram bot rendering a question) interpret raw SMS dates exactly like the Enricher does.
func ParseExpenseDate(date string, loc *time.Location) (int64, bool) {
	return parseExpenseDate(date, loc)
}
