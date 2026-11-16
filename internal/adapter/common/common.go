// Package common holds helpers shared by all CLI adapters.
package common

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

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

// NonEmptyFile reports whether path exists and has content (used for the
// "logged in" readiness signal).
func NonEmptyFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// Probe implements the `ready` subcommand used by exec readiness probes: the
// adapters listen on loopback only, which kubelet httpGet probes cannot reach.
// It returns the process exit code.
func Probe(addr string) int {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/ready")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// RequireToken wraps h so every request except GET /ready must carry
// "Authorization: Bearer <token>" (compared in constant time); otherwise it
// answers 401. An empty token disables the check (sidecar mode: loopback only).
// Both adapters use this single header, whatever the protocol they speak.
func RequireToken(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/ready" {
			h.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"missing or invalid adapter token"}}` + "\n"))
			return
		}
		h.ServeHTTP(w, r)
	})
}
