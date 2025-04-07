package wasi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/kralicky/protols/pkg/lsprpc"
	"github.com/kralicky/protols/pkg/version"
	"github.com/kralicky/tools-lite/pkg/event"
	"github.com/kralicky/tools-lite/pkg/event/core"
	"github.com/kralicky/tools-lite/pkg/event/keys"
	"github.com/kralicky/tools-lite/pkg/event/label"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
)

func Serve(ctx context.Context) error {

	fmt.Fprintf(os.Stderr, "Starting protols %s\n", version.FriendlyVersion())
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	})))
	var eventMu sync.Mutex
	event.SetExporter(func(ctx context.Context, e core.Event, lm label.Map) context.Context {
		eventMu.Lock()
		defer eventMu.Unlock()
		if event.IsError(e) {
			if err := keys.Err.Get(e); errors.Is(err, context.Canceled) {
				return ctx
			}
			var args []any
			for i := 0; e.Valid(i); i++ {
				l := e.Label(i)
				if !l.Valid() || l.Key() == keys.Msg {
					continue
				}
				key := l.Key()
				var val bytes.Buffer
				key.Format(&val, nil, l)
				args = append(args, l.Key().Name(), val.String())
			}
			slog.Error(keys.Msg.Get(e), args...)
		}
		return ctx
	})

	stream := jsonrpc2.NewHeaderStream(stdioConn{})
	conn := jsonrpc2.NewConn(stream)
	ss := lsprpc.NewStreamServer()
	return ss.ServeStream(ctx, conn)
}

// stdioConn implements net.Conn using os.Stdin for reading and os.Stdout for writing.
type stdioConn struct {
}

func (c stdioConn) Read(b []byte) (int, error) {
	return os.Stdin.Read(b)
}

func (c stdioConn) Write(b []byte) (int, error) {
	return os.Stdout.Write(b)
}

func (c stdioConn) Close() error {

	// Avoid closing os.Stdin/os.Stdout, which are managed by the runtime.
	return nil
}

func (c stdioConn) LocalAddr() net.Addr {
	return dummyAddr("stdio-local")
}

func (c stdioConn) RemoteAddr() net.Addr {
	return dummyAddr("stdio-remote")
}

func (c stdioConn) SetDeadline(t time.Time) error {
	// No-op: deadlines are not supported over stdio.
	return nil
}

func (c stdioConn) SetReadDeadline(t time.Time) error {
	// No-op
	return nil
}

func (c stdioConn) SetWriteDeadline(t time.Time) error {
	// No-op
	return nil
}

// dummyAddr is a minimal implementation of net.Addr.
type dummyAddr string

func (d dummyAddr) Network() string {
	return "stdio"
}

func (d dummyAddr) String() string {
	return string(d)
}
