// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"fmt"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/luyu6056/gnet/internal/netpoll"
	"github.com/luyu6056/tls"
	"golang.org/x/sys/unix"
)

type eventloop struct {
	idx                 int             // loop index in the server loops list
	svr                 *server         // server in loop
	codec               ICodec          // codec for TCP
	packet              []byte          // read packet buffer
	poller              *netpoll.Poller // epoll or kqueue
	connections         []*conn         // loop connections fd -> conn
	eventHandler        EventHandler    // user eventHandler
	outbufchan, outChan chan *out
	outclose            chan bool
	lazyChan            chan *conn
}

func (lp *eventloop) loopRun() {
	defer func() {
		if e := recover(); e != nil {
			fmt.Println(e)
			debug.PrintStack()
		}
		if lp.idx == 0 && lp.svr.opts.Ticker {
			close(lp.svr.ticktock)
		}
		lp.svr.signalShutdown()
	}()

	if lp.idx == 0 && lp.svr.opts.Ticker {
		go lp.loopTicker()
	}

	sniffError(lp.poller.Polling(lp.handleEvent))
}

func (lp *eventloop) loopAccept(fd int) error {
	if fd == lp.svr.ln.fd {
		if lp.svr.ln.pconn != nil {
			return lp.loopUDPIn(fd)
		}
		nfd, sa, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EAGAIN {
				return nil
			}
			return err
		}
		if !lp.svr.Isblock {
			if err := unix.SetNonblock(nfd, true); err != nil {
				return err
			}
		}

		newlp := lp.svr.subLoopGroup.getbyfd(nfd)
		c := newTCPConn(nfd, newlp, sa)
		if lp.svr.tlsconfig != nil {
			if err = c.UpgradeTls(lp.svr.tlsconfig); err != nil {
				return err
			}
		}
		newlp.poller.Trigger(func() (err error) {
			if err = newlp.poller.AddRead(c.fd); err == nil {
				index := c.fd / newlp.svr.subLoopGroup.len()
				if index >= len(newlp.connections) {
					newlp.connections = append(newlp.connections, make([]*conn, len(newlp.connections))...)
				}
				newlp.connections[index] = c
				return newlp.loopOpen(c)
			}
			return err
		})
	}

	return nil
}

func (lp *eventloop) loopOpen(c *conn) error {
	c.state = connStateOk
	c.localAddr = lp.svr.ln.lnaddr
	c.remoteAddr = netpoll.SockaddrToTCPOrUnixAddr(c.sa)
	out, action := lp.eventHandler.OnOpened(c)
	if lp.svr.opts.TCPKeepAlive > 0 {
		if _, ok := lp.svr.ln.ln.(*net.TCPListener); ok {
			_ = netpoll.SetKeepAlive(c.fd, int(lp.svr.opts.TCPKeepAlive/time.Second))
		}
	}
	if out != nil {
		c.write(out)
	}
	return lp.handleAction(c, action)
}

func (lp *eventloop) loopIn(c *conn) error {

	n, err := unix.Read(c.fd, lp.packet)
	if n == 0 || err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		c.state = connStateCloseReady
		return lp.loopCloseConn(c, err)
	}

	c.inboundBufferWrite(lp.packet[:n])

	for inFrame := c.readframe(); inFrame != nil && c.state == connStateOk; inFrame = c.readframe() {
		switch lp.eventHandler.React(inFrame, c) {
		case Close:
			c.state = connStateCloseReady
			return lp.loopCloseConn(c, nil)
		case Shutdown:
			return ErrServerShutdown
		}

	}
	return nil
}

func (lp *eventloop) loopCloseConn(c *conn, err error) error {
	if atomic.CompareAndSwapInt32(&c.state, connStateCloseReady, connStateCloseLazyout) {
		c.loop.eventHandler.OnClosed(c, err)

		c.loop.connections[c.fd/lp.svr.subLoopGroup.len()] = nil
		lp.lazyChan <- c //进行最后的输出
	}
	return nil
}

func (lp *eventloop) loopWake(c *conn) error {

	if co := lp.connections[c.fd/lp.svr.subLoopGroup.len()]; co != c {
		return nil // ignore stale wakes.
	}

	action := lp.eventHandler.React(nil, c)
	return lp.handleAction(c, action)
}

func (lp *eventloop) loopTicker() {
	var (
		delay time.Duration
		open  bool
	)
	for {
		if err := lp.poller.Trigger(func() (err error) {
			delay, action := lp.eventHandler.Tick()
			lp.svr.ticktock <- delay
			switch action {
			case None:
			case Shutdown:
				err = ErrServerShutdown
			}
			return
		}); err != nil {
			break
		}
		if delay, open = <-lp.svr.ticktock; open {
			time.Sleep(delay)
		} else {
			break
		}
	}
}

func (lp *eventloop) handleAction(c *conn, action Action) error {
	switch action {
	case None:
		return nil
	case Close:
		c.state = connStateCloseReady
		return lp.loopCloseConn(c, nil)
	case Shutdown:
		return ErrServerShutdown
	default:
		return nil
	}
}

func (lp *eventloop) loopUDPIn(fd int) error {
	n, sa, err := unix.Recvfrom(fd, lp.packet, 0)
	if err != nil || n == 0 {
		return nil
	}
	c := newUDPConn(fd, lp, sa)
	action := lp.eventHandler.React(lp.packet[:n], c)

	switch action {
	case Shutdown:
		return ErrServerShutdown
	}
	c.releaseUDP()
	return nil
}

type out struct {
	c          *conn
	b          tls.MsgBuffer
	outbufchan chan *out
	lazyChan   chan *conn
}

const (
	delay          = time.Millisecond
	sendbufDefault = 16384 //暂时设置为一个tls包大小吧
)

func (svr *server) msgOut(bufnum int) {
	if bufnum == 0 {
		bufnum = 2048
	}
	svr.outbufchan = make(chan *out, bufnum)
	svr.outChan = make(chan *out, bufnum)
	svr.lazyChan = make(chan *conn, bufnum) //产生了EAGAIN阻塞的连接
	svr.outclose = make(chan bool)

	for i := 0; i < cap(svr.outbufchan); i++ {
		svr.outbufchan <- &out{outbufchan: svr.outbufchan, lazyChan: svr.lazyChan}
	}

	go func() { //单个gorutinue 减少unix.EAGAIN cpu消耗
		var c *conn
		for {
			c = nil
			select {
			case o := <-svr.outChan:
				o.write()
			case c = <-svr.lazyChan:

			case <-svr.outclose:
				//循环至超时退出
				for {
					select {
					case o := <-svr.outChan:
						o.write()
					case c := <-svr.lazyChan:
						svr.lazyChan <- c
					case <-time.After(time.Second):
						svr.outclose <- true
						return
					}
					for i := len(svr.outChan); i > 0; i-- {
						o := <-svr.outChan
						o.write()
					}
					for i := len(svr.lazyChan); i > 0; i-- {
						c := <-svr.lazyChan
						c.lazywrite()
					}
				}
			}

			//优先从不阻塞输出
			for i := len(svr.outChan); i > 0; i-- {
				o := <-svr.outChan
				o.write()
			}
			if c != nil {
				c.lazywrite()
			}
			for i := len(svr.lazyChan); i > 0; i-- {
				c := <-svr.lazyChan
				c.lazywrite()
			}
		}
	}()
}
func (lp *eventloop) msgOut(bufnum int) {
	if bufnum == 0 {
		bufnum = 512
	}
	lp.outbufchan = make(chan *out, bufnum)
	lp.outChan = make(chan *out, bufnum)
	lp.lazyChan = make(chan *conn, bufnum) //产生了EAGAIN阻塞的连接
	lp.outclose = make(chan bool)
	for i := 0; i < cap(lp.outbufchan); i++ {
		lp.outbufchan <- &out{outbufchan: lp.outbufchan, lazyChan: lp.lazyChan}
	}

	go func() { //单个gorutinue 减少unix.EAGAIN cpu消耗

		var c *conn
		for {
			c = nil
			select {
			case o := <-lp.outChan:
				o.write()
			case c = <-lp.lazyChan:
			case <-lp.outclose:
				//循环至超时退出
				for {
					select {
					case o := <-lp.outChan:
						o.write()
					case c := <-lp.lazyChan:
						lp.lazyChan <- c
					case <-time.After(time.Second):
						lp.outclose <- true
						return
					}
					for i := len(lp.outChan); i > 0; i-- {
						o := <-lp.outChan
						o.write()
					}
					for i := len(lp.lazyChan); i > 0; i-- {
						c := <-lp.lazyChan
						c.lazywrite()
					}
				}
			}

			//优先从不阻塞输出
			for i := len(lp.outChan); i > 0; i-- {
				o := <-lp.outChan
				o.write()
			}
			if c != nil {
				c.lazywrite()
			}
			for i := len(lp.lazyChan); i > 0; i-- {
				c := <-lp.lazyChan
				c.lazywrite()
			}
		}
	}()
}

func (o *out) write() {
	c := o.c
	defer func() {
		for i := o.c.flushWaitNum; i > 0; i-- {
			if o.c.outboundBuffer != nil {
				select {
				case o.c.flushWait <- o.c.outboundBuffer.Len():
				default:
				}
			} else {
				select {
				case o.c.flushWait <- 0:
				default:
				}
			}

		}
		o.c = nil
		o.b.Reset()
		o.outbufchan <- o

	}()
	if c.state != connStateCloseOk {
		if c.tlsconn != nil {
			c.tlsconn.Write(o.b.Bytes())
			o.b.Reset()
			for c.outboundBuffer.Len() > 0 {
				n, err := unix.Write(c.fd, c.outboundBuffer.PreBytes(sendbufDefault))
				if n <= 0 || err != nil {
					if err == unix.EAGAIN {
						c.eagainNum++
						time.AfterFunc(delay*c.eagainNum, func() { o.lazyChan <- c })
						return
					}
					c.Close()
					break
				}
				c.outboundBuffer.Shift(n)
			}
		} else {
			for c.outboundBuffer.Len() > 0 {
				n, err := unix.Write(c.fd, c.outboundBuffer.PreBytes(sendbufDefault))
				if n <= 0 || err != nil {
					if err == unix.EAGAIN {
						c.outboundBuffer.Write(o.b.Bytes())
						c.eagainNum++
						time.AfterFunc(delay*c.eagainNum, func() { o.lazyChan <- c })
						return
					}
					c.Close()
					break
				}
				c.outboundBuffer.Shift(n)
			}
			for o.b.Len() > 0 {
				n, err := unix.Write(c.fd, o.b.PreBytes(sendbufDefault))
				if n <= 0 || err != nil {
					if err == unix.EAGAIN {
						c.outboundBuffer.Write(o.b.Bytes())
						c.eagainNum++
						time.AfterFunc(delay*c.eagainNum, func() { o.lazyChan <- c })
						return
					}
					c.Close()
					break
				}
				o.b.Shift(n)
			}
		}

	}

}
func (c *conn) lazywrite() {
	if c.state != connStateCloseOk {
		if c.state == connStateCloseLazyout && c.tlsconn != nil { //关闭前通知tls关闭
			c.tlsconn.CloseWrite()
		}
		for c.outboundBuffer.Len() > 0 {
			n, err := unix.Write(c.fd, c.outboundBuffer.PreBytes(sendbufDefault))
			if n <= 0 || err != nil {
				if err == unix.EAGAIN {
					c.eagainNum++
					time.AfterFunc(delay*c.eagainNum, func() { c.loop.lazyChan <- c })
					break
				}
				c.Close()
				break
			}
			c.outboundBuffer.Shift(n)
		}

		if c.state == connStateCloseLazyout { //彻底删除close的c

			c.state = connStateCloseOk
			unix.Close(c.fd)
			c.loop.poller.Delete(c.fd)
			c.releaseTCP()
			for i := c.flushWaitNum; i > 0; i-- {
				select {
				case c.flushWait <- 0:
				default:
				}
			}
		} else {
			for i := c.flushWaitNum; i > 0; i-- {
				select {
				case c.flushWait <- c.outboundBuffer.Len():
				default:
				}
			}
		}
	}

}
