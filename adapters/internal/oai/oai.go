// Package oai holds the minimal OpenAI chat-completions types and helpers
// shared by the CLI adapters.
package oai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one chat message. Content is either a string or an array of
// {"type":"text","text":...} parts.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns the textual content of the message (non-text parts are dropped).
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

// ChatRequest is the subset of POST /v1/chat/completions the adapters honour.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// Efforts are the accepted "@effort" model suffixes.
var Efforts = map[string]bool{"low": true, "medium": true, "high": true}

// SplitModel splits "model@effort" into its parts. The effort is empty when
// there is no suffix. An unknown suffix is an error.
func SplitModel(s string) (model, effort string, err error) {
	i := strings.LastIndex(s, "@")
	if i < 0 {
		return s, "", nil
	}
	model, effort = s[:i], s[i+1:]
	if model == "" || !Efforts[effort] {
		return "", "", fmt.Errorf("invalid model %q: suffix must be @low, @medium or @high", s)
	}
	return model, effort, nil
}

// Prompt flattens a conversation into (system prompt, single user prompt).
// A lone user message is passed through unchanged; longer conversations are
// rendered as a labelled transcript.
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

// ErrorBody is the OpenAI-style error envelope.
func ErrorBody(msg, typ string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": typ}})
	return b
}
