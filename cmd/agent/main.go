package main

import (
	"os"

	"github.com/nikitamishagin/budget-bot/cmd/agent/cmd"
)

// Command agent is the entrypoint for the budget-bot application.
func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
