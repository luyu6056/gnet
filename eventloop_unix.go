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
	c.opened = connStateOk
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
		c.opened = connStateCloseReady
		return lp.loopCloseConn(c, err)
	}

	c.inboundBufferWrite(lp.packet[:n])

	for inFrame := c.readframe(); inFrame != nil && c.opened == connStateOk; inFrame = c.readframe() {
		switch lp.eventHandler.React(inFrame, c) {
		case Close:
			c.opened = connStateCloseReady
			return lp.loopCloseConn(c, nil)
		case Shutdown:
			return ErrServerShutdown
		}

	}
	return nil
}

func (lp *eventloop) loopCloseConn(c *conn, err error) error {
	if atomic.CompareAndSwapInt32(&c.opened, connStateCloseReady, connStateCloseLazyout) {
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
		c.opened = connStateCloseReady
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
