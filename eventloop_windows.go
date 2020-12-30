// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build windows

package gnet

import (
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/luyu6056/tls"
)

type eventloop struct {
	ch                  chan interface{}  // command channel
	idx                 int               // loop index
	svr                 *server           // server in loop
	codec               ICodec            // codec for TCP
	connections         map[*stdConn]bool // track all the sockets bound to this loop
	eventHandler        EventHandler      // user eventHandler
	outbufchan, outChan chan *out
	outclose            chan bool
}

func (el *eventloop) loopRun() {

	var err error
	defer func() {
		if e := recover(); e != nil {
			fmt.Println(e)
			debug.PrintStack()
		}
		if el.idx == 0 && el.svr.opts.Ticker {
			close(el.svr.ticktock)
		}
		select {
		case el.svr.close <- err:
		default:
		}

		el.svr.loopWG.Done()
		el.loopEgress()
		el.svr.loopWG.Done()
	}()
	if el.idx == 0 && el.svr.opts.Ticker {
		go el.loopTicker()
	}
	for v := range el.ch {
		switch v := v.(type) {
		case error:
			err = v
		case *stdConn:
			err = el.loopAccept(v)
		case *tcpIn:
			err = el.loopRead(v)
		case *udpIn:
			err = el.loopReadUDP(v.c)
		case *stderr:
			err = el.loopError(v.c, v.err)
		case wakeReq:
			err = el.loopWake(v.c)
		case func() error:
			err = v()
		}
		if err != nil {
			return
		}
	}
}

func (el *eventloop) loopAccept(c *stdConn) error {
	el.connections[c] = true
	c.localAddr = el.svr.ln.lnaddr
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

func (el *eventloop) loopRead(ti *tcpIn) (err error) {
	c := ti.c
	c.inboundBufferWrite(ti.buf.Bytes())
	ti.buf.Reset()
	msgbufpool.Put(ti.buf)
	for inFrame := c.readframe(); inFrame != nil && c.done == 0; inFrame = c.readframe() {
		action := el.eventHandler.React(inFrame, c)
		switch action {
		case None:
		case Close:
			return el.loopClose(c)
		case Shutdown:
			return ErrServerShutdown
		}
		if err != nil {
			return el.loopClose(c)
		}
	}

	return nil
}

func (el *eventloop) loopClose(c *stdConn) error {
	atomic.StoreInt32(&c.done, 1)
	_ = c.conn.SetReadDeadline(time.Now())
	return nil
}

func (el *eventloop) loopEgress() {
	var closed bool
	for v := range el.ch {
		switch v := v.(type) {
		case error:
			if v == errCloseConns {
				closed = true
				for c := range el.connections {
					_ = el.loopClose(c)
				}
			}
		case *stderr:
			_ = el.loopError(v.c, v.err)
		}
		if len(el.connections) == 0 && closed {
			break
		}
	}
}

func (el *eventloop) loopTicker() {
	var (
		delay time.Duration
		open  bool
	)
	for {
		el.ch <- func() (err error) {
			delay, action := el.eventHandler.Tick()
			el.svr.ticktock <- delay
			switch action {
			case Shutdown:
				err = errClosing
			}
			return
		}
		if delay, open = <-el.svr.ticktock; open {
			time.Sleep(delay)
		} else {
			break
		}
	}
}

func (el *eventloop) loopError(c *stdConn, err error) (e error) {
	if _, ok := el.connections[c]; ok {
		delete(el.connections, c)
		if e = c.conn.Close(); e == nil {
			switch atomic.LoadInt32(&c.done) {
			case 0: // read error
				if err != io.EOF {
					//log.Printf("socket: %s with err: %v\n", c.remoteAddr.String(), err)
				}
			case 1: // closed
				//log.Printf("socket: %s has been closed by client\n", c.remoteAddr.String())
			}
			switch el.eventHandler.OnClosed(c, err) {
			case Shutdown:
				return errClosing
			}
			c.releaseTCP()
		}
	}
	return
}

func (el *eventloop) loopWake(c *stdConn) error {
	if c.RemoteAddr().Network() == "tcp" {
		if _, ok := el.connections[c]; !ok {
			return nil // ignore stale wakes.
		}
	}

	action := el.eventHandler.React(nil, c)

	return el.handleAction(c, action)
}

func (el *eventloop) handleAction(c *stdConn, action Action) error {
	switch action {
	case None:
		return nil
	case Close:
		return el.loopClose(c)
	case Shutdown:
		return ErrServerShutdown
	default:
		return nil
	}
}

func (el *eventloop) loopReadUDP(c *stdConn) error {
	action := el.eventHandler.React(c.inboundBuffer.Bytes(), c)
	switch action {
	case Shutdown:
		return errClosing
	}
	c.releaseUDP()
	return nil
}

type out struct {
	c *stdConn
	b tls.MsgBuffer
}

func (el *eventloop) loopOut(bufnum int) {
	if bufnum == 0 {
		bufnum = 512
	}
	el.outbufchan = make(chan *out, bufnum)
	el.outChan = make(chan *out, bufnum)
	el.outclose = make(chan bool, 1)
	for i := len(el.outbufchan); i < cap(el.outbufchan); i++ {
		el.outbufchan <- &out{}
	}

	go func() { //单个gorutinue 减少unix.EAGAIN cpu消耗

		for {
			select {
			case o := <-el.outChan:
				if o.c.outboundBuffer != nil {
					if o.c.tlsconn != nil {
						o.c.tlsconn.Write(o.b.Bytes())
						o.c.conn.Write(o.c.outboundBuffer.Bytes())
						o.c.outboundBuffer.Reset()
					} else {
						o.c.conn.Write(o.b.Bytes())
					}
					if o.c.done == 1 {
						o.c.ctx = nil
						o.c.localAddr = nil
						o.c.remoteAddr = nil
						o.c.inboundBuffer.Reset()
						o.c.outboundBuffer.Reset()
						msgbufpool.Put(o.c.outboundBuffer)
						msgbufpool.Put(o.c.inboundBuffer)
						o.c.inboundBuffer = nil
						o.c.outboundBuffer = nil
					}
				}

				o.b.Reset()
				el.outbufchan <- o
			case <-el.outclose:
				return
			}
		}
	}()
}
