// +build windows

package gnet

import (
	"errors"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/luyu6056/tls"
)

type ClientManage struct {
	*server
}
type clientdail struct {
	c           *stdConn
	clientMange *ClientManage
}

func (svr *ClientManage) Dial(network, addr string) (Conn, error) {

	if network == "tcp4" {

		netconn, err := net.Dial("tcp4", addr)
		if err != nil {
			return nil, err
		}
		el := svr.server.subLoopGroup.next()
		c := newTCPConn(netconn, el)
		el.ch <- clientdail{
			c:           c,
			clientMange: svr,
		}

		go func() {
			var packet = make([]byte, 0x10000)
			for {
				n, err := c.conn.Read(packet)
				if err != nil {
					_ = c.conn.SetReadDeadline(time.Time{})
					el.ch <- &stderr{c, err}
					return
				}
				msg := msgbufpool.Get().(*tls.MsgBuffer)
				msg.Write(packet[:n])
				el.ch <- &tcpIn{c, msg}
			}
		}()
		return c, nil
	} else {

	}
	return nil, nil
}

func (srv *ClientManage) loopOpenClient(c *stdConn, el *eventloop) error {
	el.connections[c] = true
	c.localAddr = c.conn.LocalAddr()
	c.remoteAddr = c.conn.RemoteAddr()

	out, action := el.eventHandler.OnOpened(c)
	if out != nil {
		el.eventHandler.PreWrite()
		_, _ = c.conn.Write(out)
	}
	if el.svr.opts.TCPKeepAlive > 0 {
		if c, ok := c.conn.(*net.TCPConn); ok {
			_ = c.SetKeepAlive(true)
			_ = c.SetKeepAlivePeriod(el.svr.opts.TCPKeepAlive)
		}
	}
	return el.handleAction(c, action)
}
func client(eventHandler EventHandler, options *Options) *ClientManage {

	// Figure out the correct number of loops/goroutines to use.
	numCPU := options.LoopNum
	if numCPU <= 0 {
		numCPU = runtime.NumCPU()
	}

	svr := new(server)
	svr.close = make(chan error, numCPU+1)
	svr.opts = options
	svr.tlsconfig = options.Tlsconfig
	svr.eventHandler = eventHandler
	svr.subLoopGroup = new(eventLoopGroup)
	svr.ticktock = make(chan time.Duration, 1)
	svr.cond = sync.NewCond(&sync.Mutex{})
	svr.codec = func() ICodec {
		if options.Codec == nil {
			return new(BuiltInFrameCodec)
		}
		return options.Codec
	}()

	server := Server{
		Multicore:    numCPU > 1,
		NumEventLoop: numCPU,
		ReUsePort:    options.ReusePort,
		TCPKeepAlive: options.TCPKeepAlive,
		Close: func() {
			svr.close <- errors.New("close by server.Close()")
		},
	}

	switch svr.eventHandler.OnInitComplete(server) {
	case None:
	case Shutdown:
		return nil
	}
	go svr.signalHandler()
	// Start all loops.
	svr.startLoops(numCPU)
	go svr.stop()
	return &ClientManage{svr}
}
