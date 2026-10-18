package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nikitamishagin/budget-bot/internal/app"
	"github.com/nikitamishagin/budget-bot/internal/config"
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

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		application, err := app.Build(ctx, cfg)
		if err != nil {
			return err
		}
		defer application.Close()

		sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		return telegram.Run(sigCtx, token, application.Bot)
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "config.yaml", "path to config file")
}
