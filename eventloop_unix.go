// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"fmt"
	"runtime/debug"
	"time"

	"github.com/luyu6056/gnet/pkg/errors"

	"github.com/luyu6056/gnet/internal/socket"

	"github.com/luyu6056/gnet/internal/netpoll"
	"golang.org/x/sys/unix"
)

type eventloop struct {
	idx            int             // loop index in the server loops list
	svr            *server         // server in loop
	codec          ICodec          // codec for TCP
	packet         []byte          // read packet buffer
	poller         *netpoll.Poller // epoll or kqueue
	eventHandler   EventHandler    // user eventHandler
	outChan        chan out
	lazyChan       chan *conn
	outclose       chan bool
	udpSockets     map[int]*conn
	pollAttachment *netpoll.PollAttachment
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

	sniffError(lp.startPolling(lp.handleEvent))
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
		if lp.svr.opts.TCPNoDelay {
			if err := unix.SetsockoptInt(nfd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
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
		newlp.poller.Trigger(newlp.addread, c)
	}

	return nil
}

func (lp *eventloop) loopOpen(c *conn) error {
	if c.isClient {
		return lp.loopOpenClient(c)
	}
	c.state = connStateOk
	c.localAddr = lp.svr.ln.lnaddr
	c.remoteAddr = socket.SockaddrToTCPOrUnixAddr(c.sa)
	out, action := lp.eventHandler.OnOpened(c)
	if out != nil {
		c.write(out)
	}
	return lp.handleAction(c, action)
}

func (lp *eventloop) loopIn(c *conn) (err error) {

	if err = c.readfd(); err == nil {
		for inFrame := c.readframe(); inFrame != nil && c.state == connStateOk; inFrame = c.readframe() {
			switch lp.eventHandler.React(inFrame, c) {
			case Close:
				c.state = connStateCloseReady
				return c.loopCloseConn(err)
			case Shutdown:
				return errors.ErrEngineShutdown
			}

		}
	}

	return nil
}

func (lp *eventloop) loopWake(i interface{}) error {
	c := i.(*conn)
	if co := lp.svr.connections[c.fd]; co != c {
		return nil // ignore stale wakes.
	}

	action := lp.eventHandler.React(nil, c)
	return lp.handleAction(c, action)
}

func (el *eventloop) loopTicker() {
	if el == nil {
		return
	}
	var (
		action Action
		delay  time.Duration
		timer  *time.Timer
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		delay, action = el.eventHandler.Tick()
		switch action {
		case None:
		case Shutdown:
			el.svr.close <- true
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-el.svr.close:
			el.svr.close <- true
			return
		case <-timer.C:
		}
	}
}

func (lp *eventloop) handleAction(c *conn, action Action) error {
	switch action {
	case None:
		return nil
	case Close:
		c.state = connStateCloseReady
		return c.loopCloseConn(nil)
	case Shutdown:
		return errors.ErrEngineShutdown
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
		return errors.ErrEngineShutdown
	}
	c.releaseUDP()
	return nil
}

func (el *eventloop) addread(i interface{}) (err error) {
	c := i.(*conn)
	el.svr.connectionsLock.Lock()
	for c.fd >= len(el.svr.connections) {
		el.svr.connections = append(el.svr.connections, make([]*conn, len(el.svr.connections))...)
	}
	el.svr.connectionsLock.Unlock()
	el.svr.connections[c.fd] = c
	if c.pollAttachment == nil { // UDP socket
		c.pollAttachment = netpoll.GetPollAttachment()
		c.pollAttachment.FD = c.fd
		c.pollAttachment.Callback = el.handleEvent
		if err := el.poller.AddRead(c.pollAttachment); err != nil {
			_ = unix.Close(c.fd)
			c.releaseUDP()
			return err
		}
		el.udpSockets[c.fd] = c
		return nil
	}
	if err = el.poller.AddRead(c.pollAttachment); err == nil {
		err = socket.SetKeepAlivePeriod(c.fd, int(el.svr.opts.TCPKeepAlive.Seconds()))
		return el.loopOpen(c)
	} else {
		el.svr.connections[c.fd] = nil
	}
	return err
}
