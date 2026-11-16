// Package anthropic holds the minimal Anthropic Messages API types and
// helpers used by the Claude adapter.
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one chat message. Content is either a string or an array of
// {"type":"text","text":...} blocks.
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

// Request is the subset of POST /v1/messages the adapter honours. System is a
// string or an array of text blocks.
type Request struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system"`
	Messages  []Message       `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
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

// Prompt flattens a request into (system prompt, single user prompt).
// A lone user message is passed through unchanged; longer conversations are
// rendered as a labelled transcript.
func Prompt(r Request) (system, prompt string) {
	system = Message{Content: r.System}.Text()
	turns := r.Messages
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

// Usage mirrors the Anthropic usage object.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ErrorBody is the Anthropic error envelope.
func ErrorBody(msg, typ string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	return b
}
