package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
)

const (
	codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	refreshSkew       = 5 * time.Minute
)

// authFile mirrors $CODEX_HOME/auth.json written by `codex login`.
// Unknown fields are preserved on rewrite via Raw.
type authFile struct {
	Tokens struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// tokenSource reads the PVC login, refreshes the access token in-process and
// writes the rotated tokens back so the CLI login keeps working.
type tokenSource struct {
	mu     sync.Mutex
	path   string
	client *http.Client
	tokURL string
	now    func() time.Time
}

func newTokenSource(codexHome string) *tokenSource {
	return &tokenSource{
		path:   filepath.Join(codexHome, "auth.json"),
		client: &http.Client{Timeout: 30 * time.Second},
		tokURL: codexTokenURL,
		now:    time.Now,
	}
}

// jwtExpiry returns the exp claim of a JWT, or zero when unreadable.
func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	b, err := decodeSegment(parts[1])
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

// Token returns a valid access token and the ChatGPT account id.
func (t *tokenSource) Token(ctx context.Context) (access, account string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, err := os.ReadFile(t.path)
	if err != nil {
		return "", "", fmt.Errorf("codex is not logged in: %w", err)
	}
	var a authFile
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", "", fmt.Errorf("invalid auth.json: %w", err)
	}
	if a.Tokens.AccessToken == "" {
		return "", "", errors.New("auth.json has no access token (ChatGPT login required)")
	}
	exp := jwtExpiry(a.Tokens.AccessToken)
	if !exp.IsZero() && t.now().Add(refreshSkew).Before(exp) {
		return a.Tokens.AccessToken, a.Tokens.AccountID, nil
	}
	if a.Tokens.RefreshToken == "" {
		return "", "", errors.New("access token expired and no refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.Tokens.RefreshToken},
		"client_id":     {codexClientID},
		"scope":         {"openid profile email"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, t.tokURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("token refresh failed: HTTP %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", "", errors.New("token refresh returned no access token")
	}
	a.Tokens.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		a.Tokens.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		a.Tokens.IDToken = tr.IDToken
	}
	a.LastRefresh = t.now().UTC().Format(time.RFC3339)
	// Round-trip unknown fields so the CLI's file stays intact.
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	toks, _ := generic["tokens"].(map[string]any)
	if toks == nil {
		toks = map[string]any{}
	}
	toks["access_token"], toks["refresh_token"], toks["id_token"] = a.Tokens.AccessToken, a.Tokens.RefreshToken, a.Tokens.IDToken
	generic["tokens"], generic["last_refresh"] = toks, a.LastRefresh
	out, _ := json.MarshalIndent(generic, "", "  ")
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err == nil {
		_ = os.Rename(tmp, t.path)
	}
	return a.Tokens.AccessToken, a.Tokens.AccountID, nil
}

// responses proxies POST /v1/responses to the ChatGPT Codex backend, streaming
// the SSE body through unchanged so tool calls pass natively.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		s.tokens = newTokenSource(s.CodexHome)
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "cannot read body")
		return
	}
	body, err := prepareResponsesBody(raw)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	access, account, err := s.tokens.Token(r.Context())
	if err != nil {
		writeErr(w, 503, "authentication_error", err.Error())
		return
	}
	up := s.upstream
	if up == "" {
		up = codexResponsesURL
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, up, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "codex_cli_rs")
	if account != "" {
		req.Header.Set("chatgpt-account-id", account)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		writeErr(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	if status == 401 || status == 429 {
		status = 503 // 5xx so the proxy fails over to the next target
	}
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(status)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// prepareResponsesBody applies the Codex backend's requirements: the model's
// "@effort" suffix becomes reasoning.effort, store is false and stream is true
// (the backend only serves streams), and instructions must be present.
func prepareResponsesBody(raw []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	model, _ := m["model"].(string)
	if model == "" {
		return nil, errors.New("model is required")
	}
	name, effort, err := common.SplitModel(model)
	if err != nil {
		return nil, err
	}
	m["model"] = name
	if effort != "" {
		rs, _ := m["reasoning"].(map[string]any)
		if rs == nil {
			rs = map[string]any{}
		}
		rs["effort"] = effort
		m["reasoning"] = rs
	}
	m["store"], m["stream"] = false, true
	if s, _ := m["instructions"].(string); s == "" {
		m["instructions"] = "You are a helpful assistant."
	}
	for _, k := range []string{"max_output_tokens", "temperature", "top_p", "metadata", "user", "previous_response_id"} {
		delete(m, k)
	}
	return json.Marshal(m)
}

func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
