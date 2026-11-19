package proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

type smallChunkReadCloser struct{ *strings.Reader }

func (smallChunkReadCloser) Close() error { return nil }

func (r smallChunkReadCloser) Read(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	return r.Reader.Read(p)
}

func TestAliasRestoreReadCloserHandlesChunkedMultilineSSEAndCapturesResponseID(t *testing.T) {
	const alias = "mcp_0123456789abcdef0123456789ab"
	const original = "mcp__codex_apps__codex_document_control___execute_document_command"
	input := "event: response.output_item.done\r\n" +
		"data: {\"type\":\"response.output_item.done\",\"response\":\r\n" +
		"data: {\"id\":\"resp-stream\",\"output\":[\r\n" +
		"data: {\"type\":\"function_call\",\"name\":\"" + alias + "\"}\r\n" +
		"data: ]}}\r\n\r\n"
	var capturedID string
	var capturedNames map[string]string
	body := &aliasRestoreReadCloser{
		ReadCloser: smallChunkReadCloser{Reader: strings.NewReader(input)},
		aliases:    map[string]string{alias: original},
		onResponseID: func(id string, names map[string]string) error {
			capturedID = id
			capturedNames = names
			return nil
		},
	}
	output, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if capturedID != "resp-stream" || capturedNames[alias] != original {
		t.Fatalf("captured response mapping=%q %#v", capturedID, capturedNames)
	}
	if !bytes.Contains(output, []byte(`"name":"`+original+`"`)) {
		t.Fatalf("streamed event did not restore the function name: %s", output)
	}
	if !bytes.Contains(output, []byte("event: response.output_item.done\r\n")) || !bytes.HasSuffix(output, []byte("\r\n\r\n")) {
		t.Fatalf("SSE framing was not preserved: %q", output)
	}
}

func TestResponsesContinuationFailsClosedWhenSharedAliasStoreIsUnavailable(t *testing.T) {
	store := miniredis.RunT(t)
	yml := `server: {queue_timeout: '500ms'}
coordination: {valkey: {address: '` + store.Addr() + `'}}
pools:
  api: {upstream_url: 'http://127.0.0.1:1', protocol: openai, max_concurrency: 0}
endpoints:
  openai:
    fast: {targets: [{pool: api}]}
`
	firstLoader, _ := protoLoader(t, yml)
	first := New(firstLoader.State, quiet())
	aliases := map[string]string{"mcp_0123456789abcdef0123456789ab": "mcp__codex_apps__long_document_tool_name"}
	if err := first.saveResponseNames("resp-shared", aliases); err != nil {
		t.Fatal(err)
	}
	store.Close()

	secondLoader, _ := protoLoader(t, yml)
	second := New(secondLoader.State, quiet())
	_, _, err := second.aliasResponsesRequest([]byte(`{"previous_response_id":"resp-shared","input":[{"type":"function_call_output","call_id":"call-1","output":"done"}]}`))
	if err == nil {
		t.Fatal("continuation should fail when alias state cannot be loaded")
	}
}
