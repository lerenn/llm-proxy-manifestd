package oai

import (
	"encoding/json"
	"testing"
)

func TestSplitModel(t *testing.T) {
	for _, c := range []struct{ in, m, e string; bad bool }{
		{"claude-sonnet-5-5@medium", "claude-sonnet-5-5", "medium", false},
		{"claude-haiku-5-5", "claude-haiku-5-5", "", false},
		{"x@ultra", "", "", true},
		{"@low", "", "", true},
	} {
		m, e, err := SplitModel(c.in)
		if (err != nil) != c.bad || m != c.m || e != c.e {
			t.Errorf("%q: got %q %q %v", c.in, m, e, err)
		}
	}
}

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
