package openai

import (
	"encoding/json"
	"testing"
)

func TestPrompt(t *testing.T) {
	mk := func(r, c string) Message { b, _ := json.Marshal(c); return Message{Role: r, Content: b} }
	sys, p := Prompt([]Message{mk("system", "be brief"), mk("user", "hi")})
	if sys != "be brief" || p != "hi" {
		t.Fatalf("%q %q", sys, p)
	}
	_, p = Prompt([]Message{mk("user", "a"), mk("assistant", "b"), mk("user", "c")})
	if p != "User: a\n\nAssistant: b\n\nUser: c\n\nAssistant:" {
		t.Fatalf("%q", p)
	}
	parts := Message{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"x"},{"type":"image_url"},{"type":"text","text":"y"}]`)}
	if parts.Text() != "xy" {
		t.Fatal(parts.Text())
	}
}
