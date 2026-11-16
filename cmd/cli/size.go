package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

// sizeQueue forwards the local terminal size to the remote TTY: once at
// start, then on every SIGWINCH.
type sizeQueue struct {
	fd    int
	sizes chan remotecommand.TerminalSize
	sig   chan os.Signal
	done  chan struct{}
}

func newSizeQueue(fd int) *sizeQueue {
	q := &sizeQueue{
		fd:    fd,
		sizes: make(chan remotecommand.TerminalSize, 1),
		sig:   make(chan os.Signal, 1),
		done:  make(chan struct{}),
	}
	signal.Notify(q.sig, syscall.SIGWINCH)
	q.push()
	go func() {
		for {
			select {
			case <-q.done:
				return
			case <-q.sig:
				q.push()
			}
		}
	}()
	return q
}

func (q *sizeQueue) push() {
	w, h, err := term.GetSize(q.fd)
	if err != nil || w <= 0 || h <= 0 {
		return
	}
	s := remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
	select {
	case q.sizes <- s:
	default: // a resize is already pending; drop the stale one
		select {
		case <-q.sizes:
		default:
		}
		q.sizes <- s
	}
}

// Next implements remotecommand.TerminalSizeQueue; nil ends the stream.
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case s := <-q.sizes:
		return &s
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() {
	signal.Stop(q.sig)
	close(q.done)
}
