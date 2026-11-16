// Package common holds helpers shared by all CLI adapters.
package common

import (
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
