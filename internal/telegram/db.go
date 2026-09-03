package telegram

import (
	"context"
	"fmt"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/store"
)

// storeAdapter adapts *store.Store to the ExpenseWriter interface, converting store.Candidate
// into the handler.DBCandidate shape expected by the enrichment loop.
type storeAdapter struct {
	store *store.Store
}

// openStore is the default way ExpenseWriter values are created, opening the chat's uploaded
// SQLite database file. It is a variable (not a plain function), so tests can replace it with a
// fake writer.
var openStore = func(path string) (ExpenseWriter, error) {
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return &storeAdapter{store: s}, nil
}

func (a *storeAdapter) FindCandidates(ctx context.Context, description string, limit int) ([]handler.DBCandidate, error) {
	candidates, err := a.store.FindCandidates(ctx, description, limit)
	if err != nil {
		return nil, fmt.Errorf("find db candidates: %w", err)
	}
	out := make([]handler.DBCandidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, handler.DBCandidate{
			Name:          c.Name,
			CategoryFK:    c.CategoryFK,
			SubCategoryFK: c.SubCategoryFK,
			WalletFK:      c.WalletFK,
			Income:        c.Income,
		})
	}
	return out, nil
}

func (a *storeAdapter) FindSimilar(ctx context.Context, description string) (handler.DBSimilarFields, bool, error) {
	similar, ok, err := a.store.FindSimilarTransaction(ctx, description)
	if err != nil {
		return handler.DBSimilarFields{}, false, fmt.Errorf("find similar transaction: %w", err)
	}
	if !ok {
		return handler.DBSimilarFields{}, false, nil
	}
	return handler.DBSimilarFields{
		OriginalDateDue:                 similar.OriginalDateDue,
		PairedTransactionFK:             similar.PairedTransactionFK,
		PeriodLength:                    similar.PeriodLength,
		Reoccurrence:                    similar.Reoccurrence,
		EndDate:                         similar.EndDate,
		UpcomingTransactionNotification: similar.UpcomingTransactionNotification,
		Type:                            similar.Type,
		Paid:                            similar.Paid,
		CreatedAnotherFutureTransaction: similar.CreatedAnotherFutureTransaction,
		SkipPaid:                        similar.SkipPaid,
		ObjectiveFK:                     similar.ObjectiveFK,
		ObjectiveLoanFK:                 similar.ObjectiveLoanFK,
	}, true, nil
}

func (a *storeAdapter) Insert(ctx context.Context, exp handler.EnrichedExpense) (store.InsertResult, error) {
	return a.store.Insert(ctx, exp)
}

func (a *storeAdapter) Close() error {
	return a.store.Close()
}
