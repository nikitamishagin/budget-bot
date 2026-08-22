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
)

// Client talks to an OpenAI-compatible chat completions API that supports image inputs.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient creates a Client for the given base URL (e.g. "http://127.0.0.1:8080/v1"),
// API key, model name, and request timeout.
func NewClient(baseURL, apiKey, model string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
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
		Temperature: 0,
	}

	content, err := c.chatCompletion(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	return parseExpensesJSON(content)
}

// chatCompletion sends a chat completion request and returns the raw text content of the first
// choice in the response.
func (c *Client) chatCompletion(ctx context.Context, reqBody chatRequest) (string, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
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
		return "", fmt.Errorf("call llm: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

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
	return []Expense{expense}, nil
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
