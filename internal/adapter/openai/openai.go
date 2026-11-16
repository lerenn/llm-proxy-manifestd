// Package openai holds the minimal OpenAI chat-completions types and helpers
// used by the Codex adapter.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one chat message; Content is a string or an array of
// {"type":"text","text":...} parts.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns the textual content (non-text parts are dropped).
func (m Message) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Request is the subset of POST /v1/chat/completions the adapter honours.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// Prompt flattens a conversation into (system prompt, single user prompt).
// A lone user message passes through unchanged; longer conversations become a
// labelled transcript.
func Prompt(msgs []Message) (system, prompt string) {
	var sys []string
	var turns []Message
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			sys = append(sys, m.Text())
		} else {
			turns = append(turns, m)
		}
	}
	system = strings.Join(sys, "\n\n")
	if len(turns) == 1 && turns[0].Role == "user" {
		return system, turns[0].Text()
	}
	var b strings.Builder
	for _, m := range turns {
		label := "User"
		if m.Role == "assistant" {
			label = "Assistant"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", label, m.Text())
	}
	b.WriteString("Assistant:")
	return system, b.String()
}

// Usage mirrors the OpenAI usage object.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ErrorBody is the OpenAI error envelope.
func ErrorBody(msg, typ string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": typ}})
	return b
}
