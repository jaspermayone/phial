// Package llm is a small client for an OpenAI-compatible chat endpoint.
// Extraction is the only caller.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one endpoint with one model.
type Client struct {
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

// Enabled reports whether the client has what it needs to run.
func (c *Client) Enabled() bool {
	return c != nil && c.BaseURL != "" && c.Model != "" && c.APIKey != ""
}

// Complete sends one system and one user message and returns the reply.
func (c *Client) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	if !c.Enabled() {
		return "", errors.New("llm: not configured")
	}
	body, _ := json.Marshal(map[string]any{
		"model":       c.Model,
		"max_tokens":  maxTokens,
		"temperature": 0.1,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("llm: status %d: %.300s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: unreadable reply: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("llm: empty reply")
	}
	return out.Choices[0].Message.Content, nil
}

// ExtractJSON finds the first JSON array or object in text and decodes it,
// so a reply wrapped in prose or a code fence still parses.
func ExtractJSON(text string, v any) error {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		if j := strings.Index(rest, "\n"); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.LastIndex(rest, "```"); k >= 0 {
			rest = rest[:k]
		}
		text = strings.TrimSpace(rest)
	}
	start := strings.IndexAny(text, "[{")
	if start < 0 {
		return errors.New("no JSON in reply")
	}
	open, closeC := text[start], byte(']')
	if open == '{' {
		closeC = '}'
	}
	end := strings.LastIndexByte(text, closeC)
	if end < start {
		return errors.New("unterminated JSON in reply")
	}
	return json.Unmarshal([]byte(text[start:end+1]), v)
}
