package anthropic

import (
	"encoding/json"
	"testing"
)

func TestPrompt(t *testing.T) {
	mk := func(r, c string) Message { b, _ := json.Marshal(c); return Message{Role: r, Content: b} }
	sys, p := Prompt(Request{System: json.RawMessage(`"be brief"`), Messages: []Message{mk("user", "hi")}})
	if sys != "be brief" || p != "hi" {
		t.Fatalf("%q %q", sys, p)
	}
	sys, _ = Prompt(Request{System: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`), Messages: []Message{mk("user", "x")}})
	if sys != "ab" {
		t.Fatal(sys)
	}
	_, p = Prompt(Request{Messages: []Message{mk("user", "a"), mk("assistant", "b"), mk("user", "c")}})
	if p != "User: a\n\nAssistant: b\n\nUser: c\n\nAssistant:" {
		t.Fatalf("%q", p)
	}
	parts := Message{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"x"},{"type":"image"},{"type":"text","text":"y"}]`)}
	if parts.Text() != "xy" {
		t.Fatal(parts.Text())
	}
}
