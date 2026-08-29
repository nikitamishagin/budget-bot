//go:build integration

// Package llmparser_test contains integration tests that talk to a real, locally running LLM
// inference server (llama.cpp OpenAI-compatible API). They are excluded from the default
// `go test ./...` run via the "integration" build tag, since they require external
// infrastructure and are comparatively slow/non-deterministic (LLM output). Run them explicitly
// with:
//
//	go test -tags=integration ./tests/integrated/llmparser/...
//
// Configuration is taken from the standard config file (see config.example.yaml), so the
// server URL, model name and API key never need to be hardcoded here. Override the config
// path with the BUDGET_BOT_TEST_CONFIG environment variable if needed.
package llmparser_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nikitamishagin/budget-bot/internal/config"
	"github.com/nikitamishagin/budget-bot/internal/llmparser"
)

func TestLLMParserIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "LLM Parser Integration Suite")
}

// defaultTestConfigPath points at the repository's example config, which is enough to reach the
// locally running llama.cpp server; it can be overridden via BUDGET_BOT_TEST_CONFIG for setups
// with a different config file (e.g. a real config.yaml with a non-default API key).
const defaultTestConfigPath = "../../../config.example.yaml"

// screenshotCase pairs a screenshot file (without extension) with its expected parsed expenses,
// as recorded in answers.json.
type screenshotCase struct {
	name     string
	expected []llmparser.Expense
}

var _ = Describe("Client.ParseImage against a real LLM server", func() {
	var (
		client *llmparser.Client
		cases  []screenshotCase
	)

	BeforeEach(func() {
		cfgPath := defaultTestConfigPath
		if p := os.Getenv("BUDGET_BOT_TEST_CONFIG"); p != "" {
			cfgPath = p
		}
		cfg, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred(), "load test config from %q", cfgPath)

		apiKey, err := cfg.APIKey()
		Expect(err).NotTo(HaveOccurred())

		client = llmparser.NewClient(
			cfg.LLM.BaseURL,
			apiKey,
			cfg.LLM.Model,
			time.Duration(cfg.LLM.TimeoutSeconds)*time.Second,
			cfg.LLM.MaxTokensParse,
			cfg.LLM.MaxTokensResolve,
		)

		cases = loadScreenshotCases("answers.json")
	})

	It("parses every recorded test screenshot into the expected expenses", func() {
		for _, tc := range cases {
			By(tc.name)

			imagePath := filepath.Join(".", tc.name+".jpg")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			expenses, err := client.ParseImage(ctx, imagePath)
			cancel()

			Expect(err).NotTo(HaveOccurred(), "parse %s", imagePath)
			Expect(expenses).To(HaveLen(len(tc.expected)), "expense count for %s", tc.name)

			for i, want := range tc.expected {
				got := expenses[i]
				Expect(got.Type).To(Equal(want.Type), "%s expense #%d type", tc.name, i)
				Expect(got.Amount).To(BeNumerically("~", want.Amount, 0.01), "%s expense #%d amount", tc.name, i)
				Expect(got.Currency).To(Equal(want.Currency), "%s expense #%d currency", tc.name, i)
				Expect(got.Account).To(Equal(want.Account), "%s expense #%d bank_account", tc.name, i)
				Expect(got.Description).To(Equal(want.Description), "%s expense #%d description", tc.name, i)
				Expect(got.ParseFailed).To(Equal(want.ParseFailed), "%s expense #%d parse_failed", tc.name, i)
			}
		}
	})
})

// loadScreenshotCases reads the given answers file (a JSON object mapping a screenshot name,
// without extension, to its expected list of Expense values) and returns it as a slice, sorted
// the same way the JSON keys were declared is not guaranteed by encoding/json, so tests must not
// rely on cross-case ordering.
func loadScreenshotCases(answersFile string) []screenshotCase {
	data, err := os.ReadFile(answersFile)
	Expect(err).NotTo(HaveOccurred(), "read %s", answersFile)

	var raw map[string][]llmparser.Expense
	Expect(json.Unmarshal(data, &raw)).To(Succeed(), "unmarshal %s", answersFile)

	cases := make([]screenshotCase, 0, len(raw))
	for name, expected := range raw {
		cases = append(cases, screenshotCase{name: name, expected: expected})
	}
	return cases
}
