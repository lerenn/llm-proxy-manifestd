package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

const responsesFunctionNameLimit = 64

// aliasResponsesTools applies the Responses API function-name contract at the
// shared proxy boundary, so every OpenAI-compatible provider receives valid
// names. Aliases are deterministic, allowing continuation inputs to be mapped
// without server-side session state.
func aliasResponsesTools(body []byte) ([]byte, map[string]string, error) {
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return nil, nil, err
	}
	aliases := map[string]string{}
	if tools, ok := req["tools"].([]any); ok {
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok || tool["type"] != "function" {
				continue
			}
			if name, ok := tool["name"].(string); ok {
				aliasResponsesFunctionName(tool, name, aliases)
			}
		}
	}
	if input, ok := req["input"].([]any); ok {
		for _, item := range input {
			entry, ok := item.(map[string]any)
			if !ok || entry["type"] != "function_call" {
				continue
			}
			if name, ok := entry["name"].(string); ok {
				aliasResponsesFunctionName(entry, name, aliases)
			}
		}
	}
	if choice, ok := req["tool_choice"].(map[string]any); ok && choice["type"] == "function" {
		if name, ok := choice["name"].(string); ok {
			aliasResponsesFunctionName(choice, name, aliases)
		}
	}
	if len(aliases) == 0 {
		return body, aliases, nil
	}
	out, err := json.Marshal(req)
	return out, aliases, err
}

func aliasResponsesFunctionName(item map[string]any, name string, aliases map[string]string) {
	if validResponsesFunctionName(name) {
		return
	}
	sum := sha256.Sum256([]byte(name))
	alias := "mcp_" + hex.EncodeToString(sum[:])[:28]
	aliases[alias] = name
	item["name"] = alias
}

func validResponsesFunctionName(name string) bool {
	if len(name) == 0 || len(name) > responsesFunctionNameLimit {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

type aliasRestoreReadCloser struct {
	io.ReadCloser
	aliases map[string]string
	reader  *bufio.Reader
	pending []byte
}

type terminalErrorReader struct{ err error }

func (r terminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

func (r *aliasRestoreReadCloser) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.reader == nil {
			r.reader = bufio.NewReader(r.ReadCloser)
		}
		line, err := r.reader.ReadString('\n')
		if len(line) == 0 && err != nil {
			return 0, err
		}
		r.pending = []byte(restoreResponsesSSELine(line, r.aliases))
		if len(r.pending) == 0 && err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func restoreResponsesJSON(body []byte, aliases map[string]string) []byte {
	var value any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return body
	}
	restoreResponsesNames(value, aliases)
	encoded, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return encoded
}

func restoreResponsesSSELine(line string, aliases map[string]string) string {
	if !strings.HasPrefix(line, "data: ") {
		return line
	}
	data := strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
	data = strings.TrimSuffix(data, "\r")
	if data == "[DONE]" {
		return line
	}
	var event any
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&event) != nil {
		return line
	}
	restoreResponsesNames(event, aliases)
	encoded, err := json.Marshal(event)
	if err != nil {
		return line
	}
	ending := ""
	if strings.HasSuffix(line, "\n") {
		ending = "\n"
		if strings.HasSuffix(line, "\r\n") {
			ending = "\r\n"
		}
	}
	return "data: " + string(encoded) + ending
}

func restoreResponsesNames(value any, aliases map[string]string) {
	switch v := value.(type) {
	case map[string]any:
		if name, ok := v["name"].(string); ok {
			if original, found := aliases[name]; found {
				v["name"] = original
			}
		}
		for _, child := range v {
			restoreResponsesNames(child, aliases)
		}
	case []any:
		for _, child := range v {
			restoreResponsesNames(child, aliases)
		}
	}
}
