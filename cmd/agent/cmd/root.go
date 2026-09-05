package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nikitamishagin/budget-bot/internal/config"
	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
	"github.com/nikitamishagin/budget-bot/internal/telegram"
)

var (
	cfgFile string
	cfg     *config.Config
)

var rootCmd = &cobra.Command{
	Use:   "agent",
	Short: "budget-bot is a personal expense tracking agent",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		var err error
		cfg, err = config.Load(cfgFile)
		if err != nil {
			return err
		}

		level, err := zerolog.ParseLevel(cfg.Log.Level)
		if err != nil {
			level = zerolog.InfoLevel
		}
		zerolog.SetGlobalLevel(level)
		log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).With().Timestamp().Logger()

		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := cfg.TelegramToken()
		if err != nil {
			return err
		}
		if len(cfg.Telegram.AllowedUsernames) == 0 {
			log.Warn().Msg("telegram.allowed_usernames is empty, no user will be able to use the bot")
		}

		apiKey, err := cfg.APIKey()
		if err != nil {
			return err
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

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		store, err := memory.New(ctx, cfg.DSN())
		if err != nil {
			return fmt.Errorf("connect to memory store: %w", err)
		}
		defer store.Close()

		if err := store.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate memory schema: %w", err)
		}

		enricher := handler.NewEnricher(store, llmClient, cfg.Memory.TopN, cfg.Memory.HighConfidenceThreshold)
		bot := telegram.NewBot(nil, llmClient, enricher, store, cfg.Telegram.AllowedUsernames, cfg.Telegram.DBPath)

		sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		return telegram.Run(sigCtx, token, bot)
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "config.yaml", "path to config file")
}
