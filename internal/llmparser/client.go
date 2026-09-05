package llmparser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// stopSequence is Qwen's end-of-turn token. It is always sent explicitly as a safety net against
// endless generation caused by misconfigured EOS/chat-template metadata in a quantized GGUF.
const stopSequence = "<|im_end|>"

// defaultMaxTokensParse/defaultMaxTokensResolve cap generation length so a broken stop condition
// can never run away past a bounded limit.
const (
	defaultMaxTokensParse   = 4096
	defaultMaxTokensResolve = 512
)

// Client talks to an OpenAI-compatible chat completions API that supports image inputs.
type Client struct {
	baseURL          string
	apiKey           string
	model            string
	httpClient       *http.Client
	maxTokensParse   int
	maxTokensResolve int
	reasoningEffort  string
}

// NewClient creates a Client for the given base URL (e.g. "http://127.0.0.1:8080/v1"),
// API key, model name, and request timeout. maxTokensParse/maxTokensResolve cap the number of
// generated tokens for image parsing and description resolution requests respectively; a value <= 0
// falls back to a sane default.
func NewClient(baseURL, apiKey, model string, timeout time.Duration, maxTokensParse, maxTokensResolve int, reasoningEffort string) *Client {
	if maxTokensParse <= 0 {
		maxTokensParse = defaultMaxTokensParse
	}
	if maxTokensResolve <= 0 {
		maxTokensResolve = defaultMaxTokensResolve
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		maxTokensParse:   maxTokensParse,
		maxTokensResolve: maxTokensResolve,
		reasoningEffort:  reasoningEffort,
	}
}

// chatMessage mirrors the OpenAI chat completion message format with multimodal content.
type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	// Stop is always set to stopSequence as a safety net: even if the GGUF's own EOS metadata is
	// misconfigured, the server must stop generating once it sees the model's real end-of-turn
	// token.
	Stop []string `json:"stop,omitempty"`
	// MaxTokens bounds generation length so a broken stop condition can't run away past a bounded
	// limit.
	MaxTokens int `json:"max_tokens,omitempty"`
	// TopP and TopK tune sampling determinism. TopP is left unset (0) for our use cases since it
	// has little effect at Temperature 0 anyway.
	TopP float64 `json:"top_p,omitempty"`
	TopK int     `json:"top_k,omitempty"`
	// ResponseFormat asks llama.cpp for grammar-constrained JSON output instead of relying on
	// prompt instructions alone.
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	// ChatTemplateKwargs disables Qwen3's hybrid "thinking" mode for these single-shot
	// classification/extraction tasks.
	ChatTemplateKwargs *chatTemplateKwargs `json:"chat_template_kwargs,omitempty"`
	// ReasoningEffort specifies the level of effort or complexity expected for reasoning in a chat request.
	ReasoningEffort string `json:"reasoning_effort"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatTemplateKwargs struct {
	EnableThinking bool `json:"enable_thinking"`
}

var jsonResponseFormat = &responseFormat{Type: "json_object"}
var thinking = &chatTemplateKwargs{
	EnableThinking: true,
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ParseImage sends the image found at imagePath to the LLM and returns the parsed Expenses.
// A single image may contain several bank SMS messages, in which case one Expense is returned per message.
func (c *Client) ParseImage(ctx context.Context, imagePath string) ([]Expense, error) {
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, fmt.Errorf("read image %q: %w", imagePath, err)
	}
	return c.ParseImageBytes(ctx, data, mimeTypeForPath(imagePath))
}

// ParseImageBytes sends raw image bytes with the given MIME type (e.g. "image/jpeg") to the LLM
// and returns the parsed Expenses found in the image.
func (c *Client) ParseImageBytes(ctx context.Context, imageData []byte, mimeType string) ([]Expense, error) {
	encoded := base64.StdEncoding.EncodeToString(imageData)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, encoded)

	reqBody := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{
				Role:    "system",
				Content: []contentPart{{Type: "text", Text: systemPrompt}},
			},
			{
				Role: "user",
				Content: []contentPart{
					{Type: "text", Text: userPrompt},
					{Type: "image_url", ImageURL: &imageURL{URL: dataURL}},
				},
			},
		},
		Temperature:        0,
		Stop:               []string{stopSequence},
		MaxTokens:          c.maxTokensParse,
		TopK:               1,
		ResponseFormat:     jsonResponseFormat,
		ChatTemplateKwargs: thinking,
		ReasoningEffort:    c.reasoningEffort,
	}

	content, err := c.chatCompletion(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	return parseExpensesJSON(content)
}

// chatCompletion sends a chat completion request and returns the raw text content of the first
// choice in the response. It always logs the request and the response status at info level, and
// additionally logs the full request/response bodies at debug level for troubleshooting.
func (c *Client) chatCompletion(ctx context.Context, reqBody chatRequest) (string, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	start := time.Now()
	log.Info().Str("model", reqBody.Model).Int("max_tokens", reqBody.MaxTokens).Msg("sending llm request")
	log.Debug().Str("url", url).RawJSON("request_body", body).Msg("llm request content")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Error().Err(err).Str("model", reqBody.Model).Dur("duration", time.Since(start)).Msg("llm request failed")
		return "", fmt.Errorf("call llm: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	log.Info().Str("model", reqBody.Model).Int("status", resp.StatusCode).Dur("duration", time.Since(start)).Msg("received llm response")
	// respBody is not guaranteed to be valid JSON (e.g. an upstream error page), so log it as a
	// plain string rather than RawJSON to avoid a malformed-JSON logging error.
	log.Debug().Str("response_body", string(respBody)).Msg("llm response content")

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshal llm response: %w", err)
	}
	if chatResp.Error != nil {
		return "", fmt.Errorf("llm error: %s", chatResp.Error.Message)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}

	return chatResp.Choices[0].Message.Content, nil
}

// parseExpensesJSON extracts and unmarshals the Expense JSON array (or a single object, for
// backward/robustness reasons) from the model's raw text output, tolerating surrounding Markdown
// code fences or extra text.
func parseExpensesJSON(content string) ([]Expense, error) {
	if arrStr := extractJSONArray(content); arrStr != "" {
		var expenses []Expense
		if err := json.Unmarshal([]byte(arrStr), &expenses); err == nil {
			cleanDescriptions(expenses)
			return expenses, nil
		}
	}

	jsonStr := extractJSONObject(content)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON object or array found in llm response: %s", content)
	}

	var expense Expense
	if err := json.Unmarshal([]byte(jsonStr), &expense); err != nil {
		return nil, fmt.Errorf("unmarshal expense json: %w (raw: %s)", err, jsonStr)
	}
	expenses := []Expense{expense}
	cleanDescriptions(expenses)
	return expenses, nil
}

// cleanDescriptions strips a trailing ">" (and any surrounding whitespace) from each expense's
// description. The model is instructed to keep a ">" only when it is followed by more identifier
// characters (e.g. "Wolt doo>972546"); whenever it ends up as the very last character instead, it
// is always a decorative wrapping artifact from the bank's SMS layout rather than part of the
// merchant name, so it is normalized away here regardless of what the model produced.
func cleanDescriptions(expenses []Expense) {
	for i := range expenses {
		expenses[i].Description = strings.TrimRight(strings.TrimSpace(expenses[i].Description), ">")
	}
}

// extractJSONArray returns the substring between the first '[' and the matching last ']',
// but only if that range appears before/around any top-level JSON object, i.e., the response
// is (or starts with) a JSON array rather than a single object.
func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start == -1 || end == -1 || end < start {
		return ""
	}
	objStart := strings.Index(s, "{")
	if objStart != -1 && objStart < start {
		// A JSON object starts before the array bracket, so this is not a top-level array.
		return ""
	}
	return s[start : end+1]
}

// extractJSONObject returns the substring between the first '{' and the matching last '}'.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return ""
	}
	return s[start : end+1]
}

func mimeTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "image/jpeg"
	}
}
