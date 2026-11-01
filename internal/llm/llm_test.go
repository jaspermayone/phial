package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicProvider(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request path or key", 401)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn","stop_sequence":null,"content":[{"type":"text","text":"Here you go: [{\"text\":\"uses go\"}]"}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer srv.Close()
	c := &Client{Provider: "anthropic", Model: "claude-haiku-5-5", APIKey: "k", BaseURL: srv.URL}
	if !c.Enabled() {
		t.Fatal("anthropic client without base_url override should be enabled")
	}
	out, err := c.Complete(context.Background(), "sys", "user text", 100)
	if err != nil {
		t.Fatal(err)
	}
	var cands []struct{ Text string }
	if err := ExtractJSON(out, &cands); err != nil || len(cands) != 1 || cands[0].Text != "uses go" {
		t.Fatalf("parsed %v %v", cands, err)
	}
	if got["model"] != "claude-haiku-5-5" || got["max_tokens"].(float64) != 100 {
		t.Fatalf("request = %v", got)
	}
	if sys, _ := got["system"].([]any); len(sys) != 1 || !strings.Contains(sys[0].(map[string]any)["text"].(string), "sys") {
		t.Fatalf("system = %v", got["system"])
	}
}

func TestOpenAIProviderNeedsBaseURL(t *testing.T) {
	if (&Client{Provider: "openai", Model: "m", APIKey: "k"}).Enabled() {
		t.Fatal("openai without base_url should be disabled")
	}
	if (&Client{Provider: "openai", Model: "m", APIKey: "k", BaseURL: "https://x"}).Enabled() != true {
		t.Fatal("openai with base_url should be enabled")
	}
}
