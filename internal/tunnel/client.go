package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/hashicorp/yamux"
)

// RunClient is the agent side of the tunnel. Given a WebSocket net.Conn (built
// with a context spanning the whole tunnel) and the local tool server's TCP
// address (host:port, e.g. "127.0.0.1:8000"), it:
//
//  1. builds the yamux CLIENT session over conn (exactly one Client to the CP's
//     one Server — see the package doc), then
//  2. runs an accept loop: every stream the CP opens is dialed to the local
//     tool server and the two are spliced together byte-for-byte.
//
// It blocks until the session dies (the underlying conn fails, the peer goes
// away, or ctx is cancelled — cancelling ctx fails conn, which fails Accept)
// and returns the terminating error (nil on a clean ctx-driven shutdown).
//
// A failure on any single stream (tool server down, copy error) closes only
// that stream and its TCP leg; it never tears down the whole tunnel.
func RunClient(ctx context.Context, conn net.Conn, toolServerAddr string) error {
	session, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return fmt.Errorf("tunnel: building yamux client session: %w", err)
	}
	defer session.Close()

	// If the caller cancels ctx, close the session so the in-flight Accept
	// returns promptly instead of blocking until the conn notices.
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-session.CloseChan():
		}
	}()

	for {
		stream, err := session.Accept()
		if err != nil {
			// Session is dead (conn failed, ctx cancelled, or peer closed).
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("tunnel: accept stream: %w", err)
		}
		go handleStream(stream, toolServerAddr)
	}
}

// handleStream proxies one accepted yamux stream to a fresh TCP connection to
// the local tool server, splicing both directions. It owns the lifetimes of
// both ends and always closes them. Per-stream errors are logged and swallowed
// so one bad request cannot kill the tunnel.
func handleStream(stream net.Conn, toolServerAddr string) {
	defer stream.Close()

	upstream, err := net.Dial("tcp", toolServerAddr)
	if err != nil {
		// Tool server unreachable: drop just this stream. The CP sees the
		// stream close mid-request and surfaces a 502 to its client.
		log.Printf("ecu agent: dial tool server %s: %v", toolServerAddr, err)
		return
	}
	defer upstream.Close()

	pipe(stream, upstream)
}

// pipe splices a and b bidirectionally and returns once BOTH directions have
// finished. When either copy ends (EOF or error) it half-closes the write side
// of the peer if possible (so the peer sees EOF and stops reading), then waits
// for the other direction to drain. Both conns are still hard-closed by the
// deferred Close in handleStream.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Signal EOF to dst's reader by half-closing if supported; otherwise
		// the deferred full Close handles teardown.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
