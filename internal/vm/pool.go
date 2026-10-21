package vm

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// connPool keeps a few pre-established connections to the guest agent so a
// request does not pay connection setup (vsock connect through the
// hypervisor, or a forwarded TCP handshake) on its critical path. The agent
// serves one request per connection, so pooled connections are used once.
type connPool struct {
	m      Machine
	size   int
	maxAge time.Duration
	mu     sync.Mutex
	idle   []pooledConn
	kick   chan struct{}
}

type pooledConn struct {
	c       net.Conn
	created time.Time
}

func newConnPool(ctx context.Context, m Machine, size int) *connPool {
	p := &connPool{m: m, size: size, maxAge: 30 * time.Second, kick: make(chan struct{}, 1)}
	go p.fill(ctx)
	return p
}

// get returns a ready connection, or dials one when the pool is empty.
func (p *connPool) get(ctx context.Context) (net.Conn, error) {
	p.mu.Lock()
	for len(p.idle) > 0 {
		pc := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		if time.Since(pc.created) < p.maxAge && alive(pc.c) {
			p.mu.Unlock()
			p.refill()
			return pc.c, nil
		}
		pc.c.Close()
	}
	p.mu.Unlock()
	p.refill()
	return p.m.DialAgent(ctx)
}

func (p *connPool) refill() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// fill keeps the pool topped up and recycles connections that got old (an
// agent restart would otherwise leave dead connections behind).
func (p *connPool) fill(ctx context.Context) {
	tick := time.NewTicker(p.maxAge / 2)
	defer tick.Stop()
	for {
		p.mu.Lock()
		kept := p.idle[:0]
		for _, pc := range p.idle {
			if time.Since(pc.created) < p.maxAge {
				kept = append(kept, pc)
			} else {
				pc.c.Close()
			}
		}
		p.idle = kept
		missing := p.size - len(p.idle)
		p.mu.Unlock()
		for i := 0; i < missing; i++ {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			c, err := p.m.DialAgent(dctx)
			cancel()
			if err != nil {
				break
			}
			p.mu.Lock()
			p.idle = append(p.idle, pooledConn{c, time.Now()})
			p.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			p.close()
			return
		case <-p.m.Done():
			p.close()
			return
		case <-p.kick:
		case <-tick.C:
		}
	}
}

func (p *connPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pc := range p.idle {
		pc.c.Close()
	}
	p.idle = nil
}

// alive reports whether an idle connection is still open. The agent never
// sends unsolicited data, so pending EOF means the guest side closed the
// connection while it sat in the pool (it would otherwise fail the request
// it is used for).
func alive(c net.Conn) bool {
	if closed, ok := peekClosed(c); ok {
		return !closed
	}
	// no access to the socket (VZ): a read with a very short deadline
	if err := c.SetReadDeadline(time.Now().Add(100 * time.Microsecond)); err != nil {
		return true // cannot tell; let the request find out
	}
	var b [1]byte
	_, err := c.Read(b[:])
	c.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
