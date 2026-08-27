package llmparser

import (
	"context"
	"encoding/json"
	"fmt"
)

// Candidate is a previously learned topic -> enrichment mapping, offered to the LLM as one of
// the possible matches for a newly parsed expense's topic.
type Candidate struct {
	Name          string `json:"name"`
	CategoryFK    string `json:"category_fk"`
	SubCategoryFK string `json:"sub_category_fk,omitempty"`
	WalletFK      string `json:"wallet_fk"`
	Income        bool   `json:"income"`
}

// Resolution is the LLM's decision about which candidate (if any) matches a newly parsed
// expense's topic, or a clarifying question to ask the user when none of the candidates fit.
type Resolution struct {
	// Matched indicates that MatchedIndex points to a candidate (or the new fields below are filled
	// in directly) that should be used to enrich the expense.
	Matched bool `json:"matched"`
	// MatchedIndex is the zero-based index into the candidates slice that was matched valid only
	// when Matched is true and >= 0.
	MatchedIndex int `json:"matched_index"`
	// Name, CategoryFK, SubCategoryFK, WalletFK, Income let the LLM propose enrichment fields
	// directly instead of pointing at a candidate, e.g. when it can confidently infer a category
	// from the topic itself.
	Name          string `json:"name"`
	CategoryFK    string `json:"category_fk"`
	SubCategoryFK string `json:"sub_category_fk,omitempty"`
	WalletFK      string `json:"wallet_fk"`
	Income        bool   `json:"income"`
	// NeedsUserInput is true when neither a candidate nor a confident guess is available, and the
	// user must be asked Question to supply the missing information.
	NeedsUserInput bool   `json:"needs_user_input"`
	Question       string `json:"question,omitempty"`
}

// ResolveTopic asks the LLM to match a parsed expense's topic against a short list of previously
// learned candidates, or otherwise say that the user needs to be asked for missing information.
// The candidates slice is expected to already be a short, pre-filtered top-N list (see
// MEMORY-GUIDELINES.md): the full memory table must never be passed here.
func (c *Client) ResolveTopic(ctx context.Context, expense Expense, candidates []Candidate) (Resolution, error) {
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return Resolution{}, fmt.Errorf("marshal candidates: %w", err)
	}
	expenseJSON, err := json.Marshal(expense)
	if err != nil {
		return Resolution{}, fmt.Errorf("marshal expense: %w", err)
	}

	userMsg := fmt.Sprintf(
		"Parsed expense:\n%s\n\nCandidate previously learned matches (top-N by similarity):\n%s\n\n%s",
		expenseJSON, candidatesJSON, resolvePrompt,
	)

	reqBody := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: []contentPart{{Type: "text", Text: resolveSystemPrompt}}},
			{Role: "user", Content: []contentPart{{Type: "text", Text: userMsg}}},
		},
		Temperature:        0,
		Stop:               []string{stopSequence},
		MaxTokens:          c.maxTokensResolve,
		TopK:               1,
		ResponseFormat:     jsonResponseFormat,
		ChatTemplateKwargs: thinkingOff,
	}

	content, err := c.chatCompletion(ctx, reqBody)
	if err != nil {
		return Resolution{}, err
	}

	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return Resolution{}, fmt.Errorf("no JSON object found in llm resolution response: %s", content)
	}

	var resolution Resolution
	if err := json.Unmarshal([]byte(jsonStr), &resolution); err != nil {
		return Resolution{}, fmt.Errorf("unmarshal resolution json: %w (raw: %s)", err, jsonStr)
	}
	return resolution, nil
}
