package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nikitamishagin/budget-bot/internal/handler"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
	"github.com/nikitamishagin/budget-bot/internal/memory"
)

var enrichCmd = &cobra.Command{
	Use:   "enrich [image...]",
	Short: "Parse bank SMS screenshots and enrich them into Cashew-ready expenses using memory and the LLM",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		apiKey, err := cfg.APIKey()
		if err != nil {
			return err
		}

		llmClient := llmparser.NewClient(
			cfg.LLM.BaseURL,
			apiKey,
			cfg.LLM.Model,
			time.Duration(cfg.LLM.TimeoutSeconds)*time.Second,
		)

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.LLM.TimeoutSeconds)*time.Second)
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

		var hadError bool
		for _, imagePath := range args {
			log.Info().Str("image", imagePath).Msg("parsing image")

			expenses, err := llmClient.ParseImage(ctx, imagePath)
			if err != nil {
				log.Error().Err(err).Str("image", imagePath).Msg("failed to parse image")
				hadError = true
				continue
			}

			for _, exp := range expenses {
				result, err := enricher.Enrich(ctx, exp)
				if err != nil {
					log.Error().Err(err).Str("image", imagePath).Msg("failed to enrich expense")
					hadError = true
					continue
				}

				if result.NeedsUserInput {
					fmt.Printf("Need more information: %s\n", result.Question)
					continue
				}

				out, err := json.MarshalIndent(result.Expense, "", "  ")
				if err != nil {
					log.Error().Err(err).Str("image", imagePath).Msg("failed to marshal enriched expense")
					hadError = true
					continue
				}
				fmt.Println(string(out))
			}
		}

		if hadError {
			return fmt.Errorf("one or more images failed to parse or enrich")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(enrichCmd)
}
