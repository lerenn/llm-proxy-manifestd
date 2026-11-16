package lpmctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
)

// ExecRequest is one command run in a pod container.
type ExecRequest struct {
	Namespace, Pod, Container string
	Command                   []string
	Stdin                     io.Reader
	Stdout, Stderr            io.Writer
	TTY                       bool
	// SizeQueue reports terminal resizes (TTY only, may be nil).
	SizeQueue remotecommand.TerminalSizeQueue
}

// Executor runs commands in pod containers (pods/exec). It is an interface
// so tests can fake it: the fake clientset cannot exec.
type Executor interface {
	// Exec runs the command. A command that ran but exited non-zero returns
	// an *ExitError.
	Exec(ctx context.Context, req ExecRequest) error
}

// ExitError is a command that ran and exited with a non-zero status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("command exited with status %d", e.Code) }

// KubeExecutor execs through the API server with client-go remotecommand
// (WebSocket, falling back to SPDY).
type KubeExecutor struct {
	Config *rest.Config
	Client kubernetes.Interface
}

// Exec implements Executor.
func (k *KubeExecutor) Exec(ctx context.Context, r ExecRequest) error {
	req := k.Client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(r.Namespace).Name(r.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: r.Container,
			Command:   r.Command,
			Stdin:     r.Stdin != nil,
			Stdout:    r.Stdout != nil,
			Stderr:    r.Stderr != nil && !r.TTY,
			TTY:       r.TTY,
		}, scheme.ParameterCodec)

	ws, err := remotecommand.NewWebSocketExecutor(k.Config, http.MethodGet, req.URL().String())
	if err != nil {
		return fmt.Errorf("websocket executor: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(k.Config, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("spdy executor: %w", err)
	}
	// Same fallback rule as kubectl: use SPDY when the WebSocket upgrade is
	// refused (older API servers, some proxies).
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{
		Stdin:             r.Stdin,
		Stdout:            r.Stdout,
		Tty:               r.TTY,
		TerminalSizeQueue: r.SizeQueue,
	}
	if !r.TTY {
		opts.Stderr = r.Stderr
	}
	err = ex.StreamWithContext(ctx, opts)
	var ce exec.CodeExitError
	if errors.As(err, &ce) {
		return &ExitError{Code: ce.Code}
	}
	return err
}
