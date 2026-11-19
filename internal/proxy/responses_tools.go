package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const responsesFunctionNameLimit = 64
const responsesAliasTTL = 24 * time.Hour

// aliasResponsesTools applies the Responses API function-name contract at the
// shared proxy boundary, so every OpenAI-compatible provider receives valid
// names. Aliases are deterministic, and the proxy stores original names against
// upstream response IDs so stateless continuations can restore them.
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

type storedResponseNames struct {
	names     map[string]string
	expiresAt time.Time
}

func (h *Handler) aliasResponsesRequest(body []byte) ([]byte, map[string]string, error) {
	var request struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	_ = json.Unmarshal(body, &request)
	var inherited map[string]string
	if validResponseID(request.PreviousResponseID) {
		if value, ok := h.responseNames.Load(request.PreviousResponseID); ok {
			stored := value.(storedResponseNames)
			if time.Now().Before(stored.expiresAt) {
				inherited = stored.names
			} else {
				h.responseNames.Delete(request.PreviousResponseID)
			}
		}
		if len(inherited) == 0 {
			if state := h.state(); state != nil && state.Coordinator != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				encoded, found, err := state.Coordinator.GetValue(ctx, responseAliasCacheKey(request.PreviousResponseID))
				cancel()
				if err != nil {
					h.logger.Warn("could not load Responses function aliases", "error", err)
					return nil, nil, fmt.Errorf("load Responses function aliases: %w", err)
				} else if found {
					var names map[string]string
					if json.Unmarshal([]byte(encoded), &names) == nil {
						inherited = names
						h.responseNames.Store(request.PreviousResponseID, storedResponseNames{names: names, expiresAt: time.Now().Add(responsesAliasTTL)})
					}
				}
			}
		}
	}
	aliased, names, err := aliasResponsesTools(body)
	if err != nil {
		return nil, nil, err
	}
	if len(inherited) > 0 {
		merged := make(map[string]string, len(inherited)+len(names))
		for alias, original := range inherited {
			merged[alias] = original
		}
		for alias, original := range names {
			merged[alias] = original
		}
		names = merged
	}
	return aliased, names, nil
}

func (h *Handler) saveResponseNames(responseID string, names map[string]string) error {
	if !validResponseID(responseID) || len(names) == 0 {
		return nil
	}
	copyOfNames := make(map[string]string, len(names))
	for alias, original := range names {
		copyOfNames[alias] = original
	}
	h.responseNames.Store(responseID, storedResponseNames{names: copyOfNames, expiresAt: time.Now().Add(responsesAliasTTL)})
	if state := h.state(); state != nil && state.Coordinator != nil {
		if encoded, err := json.Marshal(copyOfNames); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := state.Coordinator.SetValue(ctx, responseAliasCacheKey(responseID), string(encoded), responsesAliasTTL); err != nil {
				h.logger.Warn("could not save Responses function aliases", "error", err)
				cancel()
				return fmt.Errorf("save Responses function aliases: %w", err)
			}
			cancel()
		}
	}
	// Opportunistic cleanup keeps this response-ID cache bounded during long-lived
	// processes without adding a background goroutine.
	if h.responseNameWrites.Add(1)%64 == 0 {
		now := time.Now()
		h.responseNames.Range(func(key, value any) bool {
			if id, ok := key.(string); ok {
				if stored, ok := value.(storedResponseNames); ok && now.After(stored.expiresAt) {
					h.responseNames.Delete(id)
				}
			}
			return true
		})
	}
	return nil
}

func responseAliasCacheKey(responseID string) string { return "responses/function-names/" + responseID }

func validResponseID(responseID string) bool { return responseID != "" && len(responseID) <= 256 }

func responseIDFromJSON(body []byte) string {
	var response struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &response)
	return response.ID
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
	aliases      map[string]string
	onResponseID func(string, map[string]string) error
	reader       *bufio.Reader
	pending      []byte
	failed       error
}

type terminalErrorReader struct{ err error }

func (r terminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

func (r *aliasRestoreReadCloser) Read(p []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	if len(r.pending) == 0 {
		if r.reader == nil {
			r.reader = bufio.NewReader(r.ReadCloser)
		}
		var event []byte
		var terminalErr error
		for {
			line, err := r.reader.ReadString('\n')
			if len(line) > 0 {
				event = append(event, line...)
				if strings.TrimSpace(line) == "" {
					break
				}
			}
			if err != nil {
				terminalErr = err
				break
			}
		}
		if len(event) == 0 && terminalErr != nil {
			return 0, terminalErr
		}
		var responseID string
		r.pending, responseID = restoreResponsesSSEEvent(event, r.aliases)
		if responseID != "" && r.onResponseID != nil {
			if err := r.onResponseID(responseID, r.aliases); err != nil {
				r.failed = err
				r.pending = nil
				return 0, err
			}
		}
		if len(r.pending) == 0 && terminalErr != nil {
			return 0, terminalErr
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

func restoreResponsesSSEEvent(eventBytes []byte, aliases map[string]string) ([]byte, string) {
	text := string(eventBytes)
	lines := strings.SplitAfter(text, "\n")
	var data []string
	dataLines := 0
	for _, line := range lines {
		field := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.HasPrefix(field, "data:") {
			value := strings.TrimPrefix(field, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
			dataLines++
		}
	}
	if dataLines == 0 {
		return eventBytes, ""
	}
	payload := strings.Join(data, "\n")
	if payload == "[DONE]" {
		return eventBytes, ""
	}
	var event any
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	if dec.Decode(&event) != nil {
		return eventBytes, ""
	}
	responseID := responseIDFromEvent(event)
	restoreResponsesNames(event, aliases)
	encoded, err := json.Marshal(event)
	if err != nil {
		return eventBytes, responseID
	}
	var restored strings.Builder
	replacedData := false
	for _, line := range lines {
		field := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		ending := ""
		if strings.HasSuffix(line, "\n") {
			ending = "\n"
			if strings.HasSuffix(line, "\r\n") {
				ending = "\r\n"
			}
		}
		if strings.HasPrefix(field, "data:") {
			if !replacedData {
				restored.WriteString("data: ")
				restored.Write(encoded)
				restored.WriteString(ending)
				replacedData = true
			}
			continue
		}
		restored.WriteString(line)
	}
	return []byte(restored.String()), responseID
}

func responseIDFromEvent(event any) string {
	root, ok := event.(map[string]any)
	if !ok {
		return ""
	}
	response, ok := root["response"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := response["id"].(string)
	return id
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
