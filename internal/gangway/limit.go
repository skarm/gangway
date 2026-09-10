package gangway

import (
	"net"
	"sync"
)

// limitedListener bounds how many connections may be accepted at once. Request
// concurrency already answers excess requests with 503; this separate bound
// stops a client that opens connections without using them from exhausting the
// process file-descriptor limit. Accept blocks while the bound is reached, so
// pending connections wait in the kernel backlog rather than being dropped, and
// the server's read deadlines reclaim slots held by connections that never send
// a request.
type limitedListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// LimitConnections wraps listener so at most maxConnections accepted
// connections are open at a time.
func LimitConnections(listener net.Listener, maxConnections int) net.Listener {
	// A non-positive bound would create an unbuffered channel and block Accept
	// forever, so degrade to serving one connection at a time instead.
	if maxConnections < 1 {
		maxConnections = 1
	}

	return &limitedListener{
		Listener: listener,
		slots:    make(chan struct{}, maxConnections),
		done:     make(chan struct{}),
	}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}

	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	return &limitedConn{Conn: conn, release: sync.OnceFunc(func() { <-l.slots })}, nil
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.closeErr = l.Listener.Close()
	})

	return l.closeErr
}

// limitedConn returns its slot once, however often it is closed.
type limitedConn struct {
	net.Conn
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
