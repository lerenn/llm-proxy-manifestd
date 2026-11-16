// Command keys-manager manages the client API keys Secret. Subcommands:
//
//	init   init-container mode: it makes sure the Kubernetes Secret
// holding the client API keys has one key per client of the manifest.
//
// It creates the Secret if missing, adds a random key (llmp_ + 40 alphanumerics)
// for every client without one, never overwrites an existing key and deletes keys
// of clients no longer in the manifest. It talks to the API with plain net/http
// using the in-cluster service account. Only client names are logged.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

const (
	alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	maxAttempts = 5
	saDir       = "/var/run/secrets/kubernetes.io/serviceaccount"
)

type labels []string

func (l *labels) String() string     { return strings.Join(*l, ",") }
func (l *labels) Set(v string) error { *l = append(*l, v); return nil }

type secret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   meta              `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string]string `json:"data"`
}

type meta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = "usage: keys-manager init [--config <path> --secret <name>] [--adapter-token-secret <name>] [--namespace <ns>] [--label k=v]"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "init" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	return runInit(args[1:], stdout, stderr)
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keys-manager init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to the YAML manifest (required)")
	fs.StringVar(cfgPath, "f", "", "alias of --config")
	name := fs.String("secret", "", "name of the client-keys Secret (required)")
	ns := fs.String("namespace", "", "namespace (default: the service account's)")
	api := fs.String("api", "https://kubernetes.default.svc", "Kubernetes API base URL")
	tokenFile := fs.String("token-file", saDir+"/token", "service account token file")
	caFile := fs.String("ca-file", saDir+"/ca.crt", "CA bundle file")
	nsFile := fs.String("namespace-file", saDir+"/namespace", "namespace file")
	tokenSecret := fs.String("adapter-token-secret", "", "name of the Secret holding the shared adapter token (created if absent, never overwritten)")
	var lbls labels
	fs.Var(&lbls, "label", "label key=value for a created Secret (repeatable)")
	if fs.Parse(args) != nil {
		return 2
	}
	clientMode := *cfgPath != "" || *name != ""
	if clientMode && (*cfgPath == "" || *name == "") {
		fmt.Fprintln(stderr, "keys-manager init: --config and --secret go together")
		return 2
	}
	if !clientMode && *tokenSecret == "" {
		fmt.Fprintln(stderr, "keys-manager init: --config/--secret and/or --adapter-token-secret are required")
		return 2
	}
	var cfg *config.Config
	if clientMode {
		var err error
		cfg, err = config.LoadFile(*cfgPath)
		if err != nil {
			fmt.Fprintln(stderr, "keys-manager: config:", err)
			return 1
		}
	}
	if *ns == "" {
		b, err := os.ReadFile(*nsFile)
		if err != nil {
			fmt.Fprintln(stderr, "keys-manager: namespace:", err)
			return 1
		}
		*ns = strings.TrimSpace(string(b))
	}
	lm := map[string]string{}
	for _, l := range lbls {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			fmt.Fprintf(stderr, "keys-manager: bad -label %q\n", l)
			return 2
		}
		lm[k] = v
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if ca, err := os.ReadFile(*caFile); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	k := &kube{base: strings.TrimRight(*api, "/"), ns: *ns, client: client, tokenFile: *tokenFile}
	retry := func(f func() error) error {
		var err error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			err = f()
			if !errors.Is(err, errConflict) {
				break
			}
			fmt.Fprintf(stdout, "keys-manager: conflict, retrying (%d/%d)\n", attempt, maxAttempts)
		}
		return err
	}
	if *tokenSecret != "" {
		if err := retry(func() error { return ensureToken(k, *tokenSecret, lm, stdout) }); err != nil {
			fmt.Fprintln(stderr, "keys-manager:", err)
			return 1
		}
	}
	if clientMode {
		var names []string
		for n := range cfg.Clients {
			names = append(names, n)
		}
		sort.Strings(names)
		if err := retry(func() error { return reconcile(k, *name, names, lm, stdout) }); err != nil {
			fmt.Fprintln(stderr, "keys-manager:", err)
			return 1
		}
	}
	return 0
}

// tokenKey is the data key of the adapter token Secret.
const tokenKey = "token"

// ensureToken creates the adapter token Secret with a random token if it is
// absent (or lacks the key). An existing token is never overwritten.
func ensureToken(k *kube, name string, lbls map[string]string, out io.Writer) error {
	path := "/api/v1/namespaces/" + k.ns + "/secrets"
	code, body, err := k.do(http.MethodGet, path+"/"+name, nil)
	if err != nil {
		return err
	}
	var s secret
	exists := code == http.StatusOK
	switch {
	case exists:
		if err := json.Unmarshal(body, &s); err != nil {
			return fmt.Errorf("decode secret: %w", err)
		}
		if len(s.Data[tokenKey]) > 0 {
			fmt.Fprintln(out, "keys-manager: adapter token up to date")
			return nil
		}
	case code == http.StatusNotFound:
		s = secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque",
			Metadata: meta{Name: name, Namespace: k.ns, Labels: lbls}}
	default:
		return fmt.Errorf("get secret: HTTP %d", code)
	}
	tok, err := newKey()
	if err != nil {
		return err
	}
	if s.Data == nil {
		s.Data = map[string]string{}
	}
	s.Data[tokenKey] = base64.StdEncoding.EncodeToString([]byte(tok))
	method, p := http.MethodPost, path
	if exists {
		method, p = http.MethodPut, path+"/"+name
	}
	code, _, err = k.do(method, p, s)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusConflict:
		return errConflict // lost a race: the next attempt sees the winner's token
	case code >= 200 && code < 300:
		fmt.Fprintf(out, "keys-manager: adapter token secret %q written\n", name)
		return nil
	}
	return fmt.Errorf("write secret: HTTP %d", code)
}

var errConflict = errors.New("conflict")

type kube struct {
	base, ns, tokenFile string
	client              *http.Client
}

func (k *kube) do(method, path string, body any) (int, []byte, error) {
	tok, err := os.ReadFile(k.tokenFile) // re-read: projected tokens rotate
	if err != nil {
		return 0, nil, err
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.base+path, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

func newKey() (string, error) {
	out := make([]byte, 40)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return "llmp_" + string(out), nil
}

func reconcile(k *kube, name string, clients []string, lbls map[string]string, out io.Writer) error {
	path := "/api/v1/namespaces/" + k.ns + "/secrets"
	code, body, err := k.do(http.MethodGet, path+"/"+name, nil)
	if err != nil {
		return err
	}
	var s secret
	exists := code == http.StatusOK
	switch {
	case exists:
		if err := json.Unmarshal(body, &s); err != nil {
			return fmt.Errorf("decode secret: %w", err)
		}
	case code == http.StatusNotFound:
		s = secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque",
			Metadata: meta{Name: name, Namespace: k.ns, Labels: lbls}}
	default:
		return fmt.Errorf("get secret: HTTP %d", code)
	}
	if s.Data == nil {
		s.Data = map[string]string{}
	}
	want := map[string]bool{}
	changed := !exists
	for _, c := range clients {
		want[c] = true
		if len(s.Data[c]) == 0 {
			key, err := newKey()
			if err != nil {
				return err
			}
			s.Data[c] = base64.StdEncoding.EncodeToString([]byte(key))
			fmt.Fprintf(out, "keys-manager: generated key for client %q\n", c)
			changed = true
		}
	}
	for c := range s.Data {
		if !want[c] {
			delete(s.Data, c)
			fmt.Fprintf(out, "keys-manager: removed key of client %q (not in config)\n", c)
			changed = true
		}
	}
	if !changed {
		fmt.Fprintln(out, "keys-manager: secret up to date")
		return nil
	}
	method, p := http.MethodPost, path
	if exists {
		method, p = http.MethodPut, path+"/"+name // carries resourceVersion: optimistic lock
	}
	code, _, err = k.do(method, p, s)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusConflict:
		return errConflict
	case code >= 200 && code < 300:
		fmt.Fprintf(out, "keys-manager: secret %q written (%d clients)\n", name, len(clients))
		return nil
	}
	return fmt.Errorf("write secret: HTTP %d", code)
}
