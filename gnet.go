// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package gnet

import (
	"log"
	"net"
	"strings"
	"time"

	"github.com/luyu6056/tls"
)

// Action is an action that occurs after the completion of an event.
type Action int

const (
	// None indicates that no action should occur following an event.
	None Action = iota

	// Close closes the connection.
	Close

	// Shutdown shutdowns the server.
	Shutdown
)

// Server represents a server context which provides information about the
// running server and has control functions for managing state.
type Server struct {
	// Multicore indicates whether the server will be effectively created with multi-cores, if so,
	// then you must take care of synchronizing the shared data between all event callbacks, otherwise,
	// it will run the server with single thread. The number of threads in the server will be automatically
	// assigned to the value of runtime.NumCPU().
	Multicore bool

	// The Addr parameter is an array of listening addresses that align
	// with the addr strings passed to the Serve function.
	Addr net.Addr

	// NumLoops is the number of loops that the server is using.
	NumEventLoop int

	// ReUsePort indicates whether SO_REUSEPORT is enable.
	ReUsePort bool

	// TCPKeepAlive (SO_KEEPALIVE) socket option.
	TCPKeepAlive time.Duration

	// Close the server 在OnInitComplete会把server传递下去，建议在OnInitComplete里面把Server赋值到你的server结构体里，以调用关闭gnet的Close方法
	Close func()
}

// Conn is a interface of gnet connection.
type Conn interface {
	// Context returns a user-defined context.
	Context() (ctx interface{})

	// SetContext sets a user-defined context.
	SetContext(ctx interface{})

	// LocalAddr is the connection's local socket address.
	LocalAddr() (addr net.Addr)

	// RemoteAddr is the connection's remote peer address.
	RemoteAddr() (addr net.Addr)

	Read() []byte

	// ResetBuffer resets the inbound ring-buffer, which means all data in the inbound ring-buffer has been evicted.
	ResetBuffer()

	// ShiftN shifts "read" pointer in buffer with the given length.
	ShiftN(n int) (size int)

	// ReadN reads bytes with the given length from inbound ring-buffer and event-loop-buffer, it would move
	// "read" pointer, which means it will evict the data from buffer and it can't be revoked (put back to buffer),
	// it reads data from the inbound ring-buffer and event-loop-buffer when the length of the available data is equal
	// to the given "n", otherwise, it will not read any data from the inbound ring-buffer. So you should use this
	// function only if you know exactly the length of subsequent TCP stream based on the protocol, like the
	// Content-Length attribute in an HTTP request which indicates you how much data you should read from inbound ring-buffer.
	ReadN(n int) (size int, buf []byte)

	// BufferLength returns the length of available data in the inbound ring-buffer.
	BufferLength() (size int)

	// InboundBuffer returns the inbound ring-buffer.
	//InboundBuffer() *ringbuffer.RingBuffer

	// SendTo writes data for UDP sockets, it allows you to send data back to UDP socket in individual goroutines.
	SendTo(buf []byte) error

	// AsyncWrite writes data to client/connection asynchronously, usually you would invoke it in individual goroutines
	// instead of the event-loop goroutines.
	AsyncWrite(buf []byte) error

	// Wake triggers a React event for this connection.
	Wake() error

	Close() error

	//Data() []byte

	UpgradeTls(config *tls.Config) error

	WriteNoCodec(buf []byte) error

	//阻塞并等待所有缓冲区输出,与AsyncWrite相反
	FlushWrite(buf []byte, noCodec ...bool)
}

type (
	// EventHandler represents the server events' callbacks for the Serve call.
	// Each event has an Action return value that is used manage the state
	// of the connection and server.
	EventHandler interface {
		// OnInitComplete fires when the server is ready for accepting connections.
		// The server parameter has information and various utilities.
		OnInitComplete(server Server) (action Action)

		// OnOpened fires when a new connection has been opened.
		// The info parameter has information about the connection such as
		// it's local and remote address.
		// Use the out return value to write data to the connection.
		OnOpened(c Conn) (out []byte, action Action)

		// OnClosed fires when a connection has been closed.
		// The err parameter is the last known connection error.
		OnClosed(c Conn, err error) (action Action)

		// PreWrite fires just before any data is written to any client socket.
		PreWrite()

		// React fires when a connection sends the server data.
		// Invoke c.Read() or c.ReadN(n) within the parameter c to read incoming data from client/connection.
		// Use the out return value to write data to the client/connection.
		React(frame []byte, c Conn) (action Action)
		//React1(frame []byte, c Conn)(data []byte ,action Action)
		// Tick fires immediately after the server starts and will fire again
		// following the duration specified by the delay return value.
		Tick() (delay time.Duration, action Action)
	}

	// EventServer is a built-in implementation of EventHandler which sets up each method with a default implementation,
	// you can compose it with your own implementation of EventHandler when you don't want to implement all methods in EventHandler.
	EventServer struct {
	}
)

// OnInitComplete fires when the server is ready for accepting connections.
// The server parameter has information and various utilities.
func (es *EventServer) OnInitComplete(svr Server) (action Action) {
	return
}

// OnOpened fires when a new connection has been opened.
// The info parameter has information about the connection such as
// it's local and remote address.
// Use the out return value to write data to the connection.
func (es *EventServer) OnOpened(c Conn) (out []byte, action Action) {
	return
}

// OnClosed fires when a connection has been closed.
// The err parameter is the last known connection error.
func (es *EventServer) OnClosed(c Conn, err error) (action Action) {
	return
}

// PreWrite fires just before any data is written to any client socket.
func (es *EventServer) PreWrite() {
}

// React fires when a connection sends the server data.
// Invoke c.Read() or c.ReadN(n) within the parameter c to read incoming data from client/connection.
// Use the out return value to write data to the client/connection.
func (es *EventServer) React(frame []byte, c Conn) (action Action) {
	return
}

// Tick fires immediately after the server starts and will fire again
// following the duration specified by the delay return value.
func (es *EventServer) Tick() (delay time.Duration, action Action) {
	return
}

// Serve starts handling events for the specified addresses.
//
// Addresses should use a scheme prefix and be formatted
// like `tcp://192.168.0.10:9851` or `unix://socket`.
// Valid network schemes:
//  tcp   - bind to both IPv4 and IPv6
//  tcp4  - IPv4
//  tcp6  - IPv6
//  udp   - bind to both IPv4 and IPv6
//  udp4  - IPv4
//  udp6  - IPv6
//  unix  - Unix Domain Socket
//
// The "tcp" network scheme is assumed when one is not specified.
func Serve(eventHandler EventHandler, addr string, opts ...Option) error {
	return serve(eventHandler, addr, initOptions(opts...))
}

func Client(eventHandler EventHandler, opts ...Option) *ClientManage {
	return client(eventHandler, initOptions(opts...))
}
func parseAddr(addr string) (network, address string) {
	network = "tcp"
	address = addr
	if strings.Contains(address, "://") {
		parts := strings.Split(address, "://")
		network = parts[0]
		address = parts[1]
	}
	return
}

func sniffError(err error) {
	if err != nil {
		log.Println(err)

	}

}

type AsyncCallback func(c Conn) error
