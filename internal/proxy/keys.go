package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

// keyRefresh is how often the key directory is re-read (a mounted Secret is
// updated in place by the kubelet, so changes apply without a restart).
const keyRefresh = 5 * time.Second

type clientKey struct {
	name string
	hash [sha256.Size]byte
}

// keyStore holds the sha256 hashes of the client keys read from a directory.
// Plaintext keys are hashed immediately and not retained.
type keyStore struct {
	mu       sync.Mutex
	dir      string
	keys     []clientKey
	loadedAt time.Time
	ok       bool // last read succeeded
	nclients int
	now      func() time.Time
}

func (k *keyStore) load(dir string, clients map[string]config.ClientConfig) []clientKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now
	if now == nil {
		now = time.Now
	}
	if dir == k.dir && k.nclients == len(clients) && !k.loadedAt.IsZero() && now().Sub(k.loadedAt) < keyRefresh {
		return k.keys
	}
	k.dir, k.loadedAt, k.nclients = dir, now(), len(clients)
	entries, err := os.ReadDir(dir)
	if err != nil {
		k.keys, k.ok = nil, false
		return nil
	}
	var keys []clientKey
	for _, e := range entries {
		name := e.Name()
		// Skip the kubelet's ..data / ..timestamp bookkeeping entries.
		if strings.HasPrefix(name, ".") {
			continue
		}
		if _, known := clients[name]; !known {
			continue // a key without a client in the manifest is ignored
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) // follows the Secret symlinks
		if err != nil {
			continue
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			continue
		}
		keys = append(keys, clientKey{name: name, hash: sha256.Sum256([]byte(key))})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].name < keys[j].name })
	k.keys, k.ok = keys, true
	return keys
}

// match returns the client name for the presented key, comparing hashes in
// constant time against every configured key.
func matchKey(keys []clientKey, presented string) (string, bool) {
	h := sha256.Sum256([]byte(presented))
	name, found := "", 0
	for _, k := range keys {
		if subtle.ConstantTimeCompare(h[:], k.hash[:]) == 1 {
			name, found = k.name, 1
		}
	}
	return name, found == 1
}

// presentedKey extracts the client key: x-api-key (Anthropic clients) or
// Authorization: Bearer (OpenAI clients).
func presentedKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Api-Key")); v != "" {
		return v
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

type clientCtxKey struct{}

// clientFrom returns the authenticated client name, "" when auth is disabled.
func clientFrom(ctx context.Context) string {
	s, _ := ctx.Value(clientCtxKey{}).(string)
	return s
}

// authorize enforces client keys on /v1/ and /status. It returns false after
// having written the response (401, or 403 when the client may not use the
// endpoint). /healthz and the dashboard are never gated here.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	p := r.URL.Path
	if !(strings.HasPrefix(p, "/v1/") || p == "/status") {
		return r, true
	}
	st := h.state()
	if st == nil || len(st.Config.Clients) == 0 {
		return r, true
	}
	keys := h.keys.load(st.Config.Server.ClientKeysDir, st.Config.Clients)
	name, ok := matchKey(keys, presentedKey(r))
	if !ok {
		h.logger.Warn("rejected request: missing or invalid client key", "path", p, "remote", r.RemoteAddr)
		writeProtoError(w, protocolOf(p), http.StatusUnauthorized, "authentication_error", "invalid_api_key", "missing or invalid API key")
		return r, false
	}
	c, _ := h.clients.LoadOrStore(name, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
	h.logger.Info("request", "client", name, "method", r.Method, "path", p)
	return r.WithContext(context.WithValue(r.Context(), clientCtxKey{}, name)), true
}

// clientConfig returns the authenticated client's settings (zero when auth is off).
func clientConfig(st *config.RuntimeState, r *http.Request) config.ClientConfig {
	return st.Config.Clients[clientFrom(r.Context())]
}
