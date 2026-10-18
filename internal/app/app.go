// Package app wires the agent components together.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/nikitamishagin/budget-bot/internal/config"
	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/nikitamishagin/budget-bot/internal/telegram"
)

// App holds the fully wired agent components.
type App struct {
	Bot      *telegram.Bot
	Memory   *memory.Store
	Parser   *llmparser.Client
	Enricher *handler.Enricher
}

// Build creates the LLM client, connects to memory, migrates its schema and
// builds the enricher and the bot (with a nil Sender, set it before use).
func Build(ctx context.Context, cfg *config.Config) (*App, error) {
	apiKey, err := cfg.APIKey()
	if err != nil {
		return nil, err
	}
	llmClient := llmparser.NewClient(
		cfg.LLM.BaseURL,
		apiKey,
		cfg.LLM.Model,
		time.Duration(cfg.LLM.TimeoutSeconds)*time.Second,
		cfg.LLM.MaxTokensParse,
		cfg.LLM.MaxTokensResolve,
		cfg.LLM.ReasoningEffort,
	)

	store, err := memory.New(ctx, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("connect to memory store: %w", err)
	}

	if err := store.Migrate(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("migrate memory schema: %w", err)
	}

	enricher := handler.NewEnricher(store, llmClient, cfg.Memory.TopN, cfg.Memory.HighConfidenceThreshold)
	bot := telegram.NewBot(nil, llmClient, enricher, store, cfg.Telegram.AllowedUsernames, cfg.Telegram.DBPath)

	return &App{Bot: bot, Memory: store, Parser: llmClient, Enricher: enricher}, nil
}

// Close releases the resources held by the App.
func (a *App) Close() {
	if a != nil && a.Memory != nil {
		a.Memory.Close()
	}
}
