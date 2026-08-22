package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nikitamishagin/budget-bot/internal/llmparser"
)

var parseCmd = &cobra.Command{
	Use:   "parse [image...]",
	Short: "Parse one or more bank SMS screenshots into structured expense data using the local LLM",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		apiKey, err := cfg.APIKey()
		if err != nil {
			return err
		}

		client := llmparser.NewClient(
			cfg.LLM.BaseURL,
			apiKey,
			cfg.LLM.Model,
			time.Duration(cfg.LLM.TimeoutSeconds)*time.Second,
		)

		var hadError bool
		for _, imagePath := range args {
			log.Info().Str("image", imagePath).Msg("parsing image")

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.LLM.TimeoutSeconds)*time.Second)
			expenses, err := client.ParseImage(ctx, imagePath)
			cancel()
			if err != nil {
				log.Error().Err(err).Str("image", imagePath).Msg("failed to parse image")
				hadError = true
				continue
			}

			out, err := json.MarshalIndent(expenses, "", "  ")
			if err != nil {
				log.Error().Err(err).Str("image", imagePath).Msg("failed to marshal expenses")
				hadError = true
				continue
			}
			fmt.Println(string(out))
		}

		if hadError {
			return fmt.Errorf("one or more images failed to parse")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(parseCmd)
}
