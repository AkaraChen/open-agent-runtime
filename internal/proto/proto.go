// Package proto implements the Host <-> Guest Agent control protocol.
//
// Every request uses its own stream connection (vsock, unix socket or TCP):
//
//	client: <Request JSON>\n [BodySize raw bytes]
//	server: <Response JSON>\n [BodySize raw bytes | stream until EOF if -1]
//
// Bodies carry artifact tarballs (host -> guest) and logs (guest -> host).
package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Version of the wire protocol.
const Version = 2

// Operation names.
const (
	OpInfo       = "info"
	OpWarmStatus = "warm.status"
	OpWarmPut    = "warm.put"
	OpCompose    = "compose"
	OpRun        = "run"
	OpReload     = "reload"
	OpStop       = "stop"
	OpDelete     = "delete"
	OpList       = "list"
	OpState      = "state"
	OpLogs       = "logs"
	OpExec       = "exec"
	OpShutdown   = "shutdown"
	OpStats      = "stats"
	OpPrepare    = "prepare"
	OpDial       = "dial"
	OpIngress    = "ingress.list"
)

// Request header.
type Request struct {
	Op       string          `json:"op"`
	Args     json.RawMessage `json:"args,omitempty"`
	BodySize int64           `json:"bodySize,omitempty"`
}

// Response header. BodySize -1 means the body streams until EOF.
type Response struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	BodySize int64           `json:"bodySize,omitempty"`
}

// Dialer opens a new stream to the agent.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client issues requests over fresh connections.
type Client struct {
	Dial Dialer
}

// RemoteError is an error reported by the agent.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return e.Msg }

// Call sends a request with an optional body and decodes the result into
// out (if non-nil). The response body, if any, is discarded.
func (c *Client) Call(ctx context.Context, op string, args, out any) error {
	return c.CallBody(ctx, op, args, nil, 0, out, nil)
}

// CallBody is Call with a request body and an optional response body sink.
func (c *Client) CallBody(ctx context.Context, op string, args any, body io.Reader, size int64, out any, sink io.Writer) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return fmt.Errorf("connect to agent: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	req := Request{Op: op, BodySize: size}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		req.Args = b
	}
	hdr, _ := json.Marshal(req)
	w := bufio.NewWriterSize(conn, 1<<20)
	if _, err := w.Write(append(hdr, '\n')); err != nil {
		return err
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	sendErr := func(err error) error {
		// the agent may have rejected the request early; prefer its reason
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if line, rerr := r.ReadBytes('\n'); rerr == nil {
			var resp Response
			if json.Unmarshal(line, &resp) == nil && !resp.OK && resp.Error != "" {
				return &RemoteError{Msg: resp.Error}
			}
		}
		return err
	}
	if body != nil && size > 0 {
		if _, err := io.CopyN(w, body, size); err != nil {
			return sendErr(fmt.Errorf("send body: %w", err))
		}
	}
	if err := w.Flush(); err != nil {
		return sendErr(err)
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !resp.OK {
		return &RemoteError{Msg: resp.Error}
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return err
		}
	}
	if sink != nil {
		switch {
		case resp.BodySize > 0:
			_, err = io.CopyN(sink, r, resp.BodySize)
		case resp.BodySize < 0:
			_, err = io.Copy(sink, r)
			if ctx.Err() != nil || isClosed(err) {
				err = nil
			}
		}
	}
	return err
}

func isClosed(err error) bool {
	return err != nil && (errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "deadline") || errors.Is(err, io.EOF))
}

// Stream opens a bidirectional byte stream (port forwarding). It does not
// wait for the agent's response: the caller can write right away, which
// saves a round trip per connection on slow channels. The response header
// is checked on the first Read; a refused stream fails that Read.
func (c *Client) Stream(ctx context.Context, op string, args any) (net.Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
	req := Request{Op: op}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			conn.Close()
			return nil, err
		}
		req.Args = b
	}
	hdr, _ := json.Marshal(req)
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	return &streamConn{Conn: conn, r: bufio.NewReader(conn)}, nil
}

type streamConn struct {
	net.Conn
	r    *bufio.Reader
	once sync.Once
	err  error // response error, set by the first Read
}

func (s *streamConn) Read(p []byte) (int, error) {
	s.once.Do(func() {
		line, err := s.r.ReadBytes('\n')
		if err != nil {
			s.err = err
			return
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			s.err = err
		} else if !resp.OK {
			s.err = &RemoteError{Msg: resp.Error}
		}
	})
	if s.err != nil {
		return 0, s.err
	}
	return s.r.Read(p)
}

// CloseWrite half-closes the underlying connection when supported.
func (s *streamConn) CloseWrite() error {
	if cw, ok := s.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Handler serves one request. body is limited to req.BodySize bytes. It
// returns the result; if it wants to stream a response body it writes to
// the ResponseStream after calling Begin.
type Handler func(ctx context.Context, req *Request, body io.Reader, rs *ResponseStream) (any, error)

// ResponseStream lets handlers stream a response body.
type ResponseStream struct {
	conn    net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	started bool
}

// Hijack writes a successful header and hands the raw connection to the
// handler for a bidirectional byte stream (port forwarding). The returned
// reader holds bytes the client may already have sent.
func (rs *ResponseStream) Hijack(result any) (net.Conn, *bufio.Reader, error) {
	if err := rs.header(Response{OK: true}, result); err != nil {
		return nil, nil, err
	}
	rs.started = true
	return rs.conn, rs.r, nil
}

// Begin writes a successful header with the given result and returns a
// writer for the body. size -1 streams until the handler returns.
func (rs *ResponseStream) Begin(result any, size int64) (io.Writer, error) {
	if err := rs.header(Response{OK: true, BodySize: size}, result); err != nil {
		return nil, err
	}
	rs.started = true
	return flushWriter{rs.w}, nil
}

type flushWriter struct{ w *bufio.Writer }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.w.Flush()
	}
	return n, err
}

func (rs *ResponseStream) header(resp Response, result any) error {
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		resp.Result = b
	}
	b, _ := json.Marshal(resp)
	if _, err := rs.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return rs.w.Flush()
}

// Serve handles a single connection.
func Serve(ctx context.Context, conn net.Conn, h Handler) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	rs := &ResponseStream{conn: conn, r: r, w: bufio.NewWriter(conn)}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		rs.header(Response{Error: "bad request: " + err.Error()}, nil)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body := io.LimitReader(r, max(req.BodySize, 0))
	result, err := h(ctx, &req, body, rs)
	if rs.started {
		rs.w.Flush()
		return
	}
	if err != nil {
		rs.header(Response{Error: err.Error()}, nil)
		return
	}
	rs.header(Response{OK: true}, result)
}

// Listen serves connections from l until ctx is done.
func Listen(ctx context.Context, l net.Listener, h Handler) error {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go Serve(ctx, conn, h)
	}
}

// Decode unmarshals request args.
func Decode(req *Request, v any) error {
	if len(req.Args) == 0 {
		return nil
	}
	return json.Unmarshal(req.Args, v)
}

// pipeBufSize is Pipe's copy buffer for non-TCP connections (vsock, the
// control channel). On vsock loopback 256 KiB doubled throughput over
// io.Copy's 32 KiB; 1 MiB was slower again.
const pipeBufSize = 256 << 10

var pipeBufs = sync.Pool{New: func() any { b := make([]byte, pipeBufSize); return &b }}

// Pipe copies a <-> b until both directions finish, then closes both. When
// one source ends, the other side is half-closed if it supports that.
func Pipe(a, b net.Conn) {
	done := make(chan struct{})
	go func() { copyHalf(a, b); close(done) }()
	copyHalf(b, a)
	<-done
	a.Close()
	b.Close()
}

func copyHalf(dst, src net.Conn) {
	_, tcpSrc := src.(*net.TCPConn)
	_, tcpDst := dst.(*net.TCPConn)
	if tcpSrc && tcpDst {
		io.Copy(dst, src) // splice(2)
	} else {
		bp := pipeBufs.Get().(*[]byte)
		io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *bp)
		pipeBufs.Put(bp)
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
