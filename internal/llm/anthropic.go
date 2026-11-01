package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// completeAnthropic sends the prompt through the official Claude SDK.
// Thinking stays at the model's default (adaptive on current models), and
// the reply is the text blocks joined.
func (c *Client) completeAnthropic(ctx context.Context, system, user string, maxTokens int) (string, error) {
	opts := []option.RequestOption{option.WithAPIKey(c.APIKey)}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	if c.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTP))
	}
	client := anthropic.NewClient(opts...)
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.Model,
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("llm: anthropic: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", errors.New("llm: anthropic: the model declined this session")
	}
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 {
		return "", errors.New("llm: anthropic: empty reply")
	}
	return b.String(), nil
}
