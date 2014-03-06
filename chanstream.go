// Copyright 2014 Garrett D'Amore
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License. 
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package chanstream provides an API that is similar to that used for TCP
// and Unix Domain sockets (see net.TCP), for use in intra-process
// communication on top of Go channels.  This makes it easy to swap it for
// another net.Conn interface. 
//
// By using channels, we avoid exposing any
// interface to other processors, or involving the kernel to perform data
// copying.

package chanstream

import "net"
import "sync"
import "time"
import "io"

// ChanErr implements the error and net.Error interfaces.
type ChanError struct {
	err string
	tmo bool
	tmp bool
}

func (e *ChanError) Error() string {
	return e.err
}

func (e *ChanError) Timeout() bool {
	return e.tmo
}

func (e *ChanError) Temporary() bool {
	return e.tmp
}

var (
	ERR_REFUSED = &ChanError{err: "Connection refused."}
	ERR_ADDRINUSE = &ChanError{err: "Address in use."}
	ERR_ACCTIME = &ChanError{err: "Accept timeout.", tmo: true}
	ERR_QFULL = &ChanError{err: "Listen queue full.", tmp: true}
	ERR_CLOSED = &ChanError{err: "Connection closed."}
	ERR_CONTIME = &ChanError{err: "Connection timeout.", tmo: true}
	ERR_RDTIME = &ChanError{err: "Read timeout.", tmo: true, tmp: true}
	ERR_WRTIME = &ChanError{err: "Write timeout.", tmo: true, tmp: true}
)

// listeners acts as a registry of listeners.
var listeners struct {
	mtx sync.Mutex
	lst map[string]*ChanListener
}

// We store just the address, which will normally be something
// like a path, but any valid string can be used as a key.  This implements
// the net.Addr interface.
type ChanAddr struct {
	name	string
}

// String returns the name of the end point -- the listen address.  This
// is just an arbitrary string used as a lookup key.
func (a *ChanAddr) String() string {
	return a.name
}

// Network returns "chan".
func (a *ChanAddr) Network() string {
	return "chan"
}

// ChanConn represents a logical connection between two peers communication
// using a pair of cross-connected go channels. This provides net.Conn
// semantics on top of channels.
type ChanConn struct {
	fifo		chan []byte
	fin		chan bool
	rdeadline	time.Time
	wdeadline	time.Time
	peer		*ChanConn
	pending		[]byte
	closed		bool
	addr		*ChanAddr
}

type chanConnect struct {
	conn 		*ChanConn
	connected	chan bool
}

// ChanListener is used to listen to a socket.
type ChanListener struct {
	name		string
	connect		chan *chanConnect
	deadline	time.Time
}

// ListenChan establishes the server address and receiving
// channel where clients can connect.  This service address is backed
// by a go channel.
func ListenChan(name string) (*ChanListener, error) {
	listeners.mtx.Lock()
	defer listeners.mtx.Unlock()

	if listeners.lst == nil {
		listeners.lst = make(map[string]*ChanListener)
	}
	if _, ok := listeners.lst[name]; ok {
		return nil, ERR_ADDRINUSE
	}

	listener := new(ChanListener)
	listener.name = name
	// The listen backlog we support.. fairly arbitrary
	listener.connect = make(chan *chanConnect, 32)
	// Register listener on the service point
	listeners.lst[name] = listener
	return listener, nil
}

// AcceptChan accepts a client's connection request via Dial,
// and returns the associated underlying connection.
func (listener *ChanListener) AcceptChan() (*ChanConn, error) {

	deadline := mkTimer(listener.deadline)

	select {
	case connect := <-listener.connect:
		// Make a pair of channels, and twist them.  We keep
		// the first pair, client gets the twisted pair.
		// We support buffering up to 10 messages for efficiency
		chan1 := make(chan []byte, 10)
		chan2 := make(chan []byte, 10)
		fin1 := make(chan bool)
		fin2 := make(chan bool)
		addr := &ChanAddr{name: listener.name}
		server := &ChanConn{fifo: chan1, fin: fin1, addr: addr}
		client := &ChanConn{fifo: chan2, fin: fin2, addr: addr}
		server.peer = client
		client.peer = server
		// And send the client its info, and a wakeup
		connect.conn = client
		connect.connected <- true
		return server, nil
		
	case <-deadline:
		// NB: its never possible to read from a nil channel.
		// So this only counts if we have a timer running.
		return nil, ERR_ACCTIME
	}
}

// Accept is a generic way to accept a connection.
func (listener *ChanListener) Accept() (net.Conn, error) {
	c, err := listener.AcceptChan()
	return c, err
}

// DialChan is the client side, think connect().
func DialChan(name string) (*ChanConn, error) {
	var listener *ChanListener
	listeners.mtx.Lock()
	if listeners.lst != nil {
		listener = listeners.lst[name]
	}
	listeners.mtx.Unlock()
	if listener == nil {
		return nil, ERR_REFUSED
	}

	// TBD: This deadline is rather arbitrary
	deadline := time.After(time.Second * 10)
	creq := &chanConnect{conn: nil}
	creq.connected = make(chan bool)

	// Note: We assume the buffering is sufficient.  If the server
	// side cannot keep up with connect requests, then we'll fail.  The
	// connect is "non-blocking" in this regard.  As there is a reasonable
	// listen backlog, this should only happen if lots of clients try to
	// connect too fast.  In TCP world if this happens it becomes
	// ECONNREFUSED.  We use ERR_QFULL.
	select {
	case listener.connect <- creq:

	default:
		return nil, ERR_QFULL
	}

	select {
	case _, ok := <-creq.connected:
		if !ok {
			return nil, ERR_CLOSED
		}

	case <-deadline:
		return nil, ERR_CONTIME
	}

	return creq.conn, nil
}

// Close implements the io.Closer interface.  It closes the channel for
// communications.  Messages that have already been sent may be received
// by the peer still.
func (conn *ChanConn) Close() error {
	conn.CloseRead()
	conn.CloseWrite()
	return nil
}

func (conn *ChanConn) CloseRead() error {
	close(conn.fin)
	conn.closed = true
	return nil
}

func (conn *ChanConn) CloseWrite() error {
	close(conn.fifo)
	return nil
}

func (conn *ChanConn) LocalAddr() net.Addr {
	return conn.addr
}

func (conn *ChanConn) RemoteAddr() net.Addr {
	return conn.peer.addr
}

func (conn *ChanConn) SetDeadline(t time.Time) error {
	conn.rdeadline = t
	conn.wdeadline = t
	return nil
}

func (conn *ChanConn) SetReadDeadline(t time.Time) error {
	conn.rdeadline = t
	return nil
}

func (conn *ChanConn) SetWriteDeadline(t time.Time) error {
	conn.wdeadline = t
	return nil
}

// Read implements the io.Reader interface.
func (conn *ChanConn) Read(b []byte) (int, error) {
	b = b[0:0] // empty slice
	for ; len(b) < cap(b); {

		// get a byte slice from our peer if we don't have one yet
		if conn.pending == nil || len(conn.pending) == 0 {
			timer := mkTimer(conn.rdeadline)
			select {
			case msg := <-conn.peer.fifo:
				if msg != nil {
					conn.pending = msg
				} else if (len(b) > 0) {
					return len(b), nil
				} else {
					return 0, io.EOF
				}

			case <-timer:
				// Timeout
				return len(b), ERR_RDTIME
			}
		}

		if conn.closed {
			return len(b), io.EOF
		}
		want := cap(b) - len(b)
		if want > len(conn.pending) {
			want = len(conn.pending)
		}
		b = append(b, conn.pending[:want]...)
		conn.pending = conn.pending[want:]
	}
	return len(b), nil
}

func (conn *ChanConn) Write(b []byte) (int, error) {
	// Unlike Read, Write is quite a bit simpler, since
	// we don't have to deal with buffers.  We just write to the
	// channel/fifo.  We do have to respect when the peer has notified
	// us that its side is closed, however.

	deadline := mkTimer(conn.wdeadline)
	n := len(b)

	select {
	case <-conn.peer.fin:
		// Remote close
		return n, ERR_CLOSED

	case conn.fifo<-b:
		// Sent it
		return n, nil

	case <-deadline:
		// Timeout
		return n, ERR_WRTIME
	}
}

// ReaderFrom, WriterTo interfaces can give some better performance,
// but we skip that for now, they're optional interfaces
// TO Add  Read, Write, (CloseRead, CloseWrite)
// ReadFrom, WriteTo, 
func mkTimer(deadline time.Time) <-chan time.Time {

	if deadline.IsZero() {
		return nil
	}

	dur := deadline.Sub(time.Now())
	if dur < 0 {
		// a closed channel never blocks
		tm := make(chan time.Time)
		close(tm)
		return tm
	}
			
	return time.After(dur)
}
