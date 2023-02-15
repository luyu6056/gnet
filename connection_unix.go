// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luyu6056/gnet/internal/netpoll"
	"github.com/luyu6056/gnet/internal/socket"
	"github.com/luyu6056/gnet/pkg/pool/byteslice"

	"github.com/luyu6056/tls"
	"golang.org/x/sys/unix"
)

const (
	connStateOk         = 2
	connStateCloseReady = 1
	connStateNone       = 0
	connStateClosed     = -1
	connStateDelete     = -2
)

var devnummfd = func() int {
	f, err := os.OpenFile("/dev/null", os.O_RDWR, 0666)
	if err != nil {
		return -1
	}
	return int(f.Fd())
}()
var msgbufpool = sync.Pool{New: func() interface{} {
	return &tls.MsgBuffer{}
}}

type conn struct {
	fd             int            // file descriptor
	sa             unix.Sockaddr  // remote socket address
	ctx            interface{}    // user-defined context
	loop           *eventloop     // connected loop
	codec          ICodec         // codec for TCP
	state          int32          // connection opened event fired
	localAddr      net.Addr       // local addr
	remoteAddr     net.Addr       // remote addr
	inboundBuffer  *tls.MsgBuffer // buffer for data from client
	outboundBuffer *tls.MsgBuffer
	tlsconn        *tls.Conn
	readfd         func() error
	readframe      func() []byte
	eagainNum      time.Duration
	flushWaitNum   int64
	flushWait      chan int
	isClient       bool
	pollAttachment *netpoll.PollAttachment // connection attachment for poller
	writetimeout   int
}

func newTCPConn(fd int, lp *eventloop, sa unix.Sockaddr) *conn {
	c := &conn{
		fd:             fd,
		sa:             sa,
		loop:           lp,
		codec:          lp.codec,
		inboundBuffer:  msgbufpool.Get().(*tls.MsgBuffer),
		outboundBuffer: msgbufpool.Get().(*tls.MsgBuffer),
		flushWait:      make(chan int, 1),
		writetimeout:   lp.srv.opts.WriteTimeOut,
	}

	c.readfd = c.tcpread
	c.readframe = c.read
	c.inboundBuffer.Reset()
	c.outboundBuffer.Reset()
	c.pollAttachment = new(netpoll.PollAttachment)
	c.pollAttachment.FD, c.pollAttachment.Callback = fd, c.handleEvents
	return c
}

func (c *conn) releaseTCP() {

	c.loop.poller.Trigger(func(i interface{}) error {
		if atomic.CompareAndSwapInt32(&c.state, connStateClosed, connStateDelete) {
			c.loop.srv.connections.Delete(c.fd)
			c.loop.poller.Delete(c.fd)

			if c.inboundBuffer != nil {
				c.inboundBuffer.Reset()
				msgbufpool.Put(c.inboundBuffer)
				c.inboundBuffer = nil
			}
			if c.outboundBuffer != nil {
				c.outboundBuffer.Reset()
				msgbufpool.Put(c.outboundBuffer)
				c.outboundBuffer = nil
			}

			c.loop.srv.connWg.Done()
			c.fd = devnummfd

		}
		return nil
	}, nil)

	//netpoll.PutPollAttachment(c.pollAttachment)
	//c.pollAttachment = nil
}

// 优化buffer
func (c *conn) readfdF() error {
	return c.readfd()
}

var conn_m sync.Map

func newUDPConn(fd int, lp *eventloop, sa unix.Sockaddr) (c *conn) {
	if v, ok := conn_m.Load(socket.SockaddrToUDPAddr(sa).String()); ok {
		c = v.(*conn)
	} else {
		c = &conn{
			localAddr:  lp.srv.ln.lnaddr,
			remoteAddr: socket.SockaddrToUDPAddr(sa),
		}
	}
	c.fd = fd
	c.sa = sa
	c.loop = lp
	return
}

func (c *conn) releaseUDP() {

}
func (c *conn) tcpread() error {
	n, err := unix.Read(c.fd, c.inboundBuffer.Make(4096))
	c.inboundBuffer.Truncate(c.inboundBuffer.Len() - 4096 + n)
	if n == 0 || err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		c.loopCloseConn(err)
		return err
	}
	return nil
}
func (c *conn) tlsread() (frame []byte) {

	var err error
	if !c.tlsconn.HandshakeComplete() {
		//先判断是否足够一条消息
		data := c.tlsconn.RawData()
		if len(data) < 5 || len(data) < 5+int(data[3])<<8|int(data[4]) {
			return
		}
		if err = c.tlsconn.Handshake(); err != nil {
			c.ErrClose(err)
			return
		}
		if !c.tlsconn.HandshakeComplete() || len(c.tlsconn.RawData()) == 0 { //握手没成功，或者握手成功，但是没有数据黏包了
			return
		}
	}

	for err = c.tlsconn.ReadFrame(); err == nil; err = c.tlsconn.ReadFrame() { //循环读取直到获得
		frame, err = c.codec.Decode(c)
		if err != nil {
			c.ErrClose(err)
			return
		}
		if frame != nil {
			return
		}
	}
	return
}
func (c *conn) read() []byte {
	frame, err := c.codec.Decode(c)
	if err != nil {
		c.ErrClose(err)
		return nil
	}
	return frame
}
func (c *conn) write(data []byte) {
	buf := byteslice.Get(len(data))
	copy(buf, data)
	c.loop.outChan <- out{c, buf}

}

func (c *conn) sendTo(buf []byte) {
	c.write(buf)
}

// ================================= Public APIs of gnet.Conn =================================

func (c *conn) Read() []byte {
	return c.inboundBuffer.Bytes()
}

func (c *conn) ResetBuffer() {
	c.inboundBuffer.Reset()
}

func (c *conn) ShiftN(n int) (size int) {
	c.inboundBuffer.Shift(n)
	return
}

func (c *conn) ReadN(n int) (size int, buf []byte) {
	buf = c.inboundBuffer.PreBytes(n)
	return len(buf), buf
}

func (c *conn) BufferLength() int {
	return c.inboundBuffer.Len()
}

// 用于直出不编码的出口，tls调用
func (c *conn) Write(data []byte) (n int, err error) {
	c.write(data)
	return len(data), nil
}

func (c *conn) AsyncWrite(buf []byte) error {

	encodedBuf, err := c.codec.Encode(c, buf)
	if len(encodedBuf) > 0 {
		c.write(encodedBuf)
	} else if err != nil {
		c.ErrClose(err)
	}
	return err
}
func (c *conn) WriteNoCodec(buf []byte) error {
	c.write(buf)
	return nil
}
func (c *conn) SendTo(buf []byte) error {
	return unix.Sendto(c.fd, buf, 0, c.sa)
}

func (c *conn) Wake() error {
	return c.loop.poller.Trigger(c.loop.loopWake, c)
}

func (c *conn) Context() interface{}       { return c.ctx }
func (c *conn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *conn) LocalAddr() net.Addr        { return c.localAddr }
func (c *conn) RemoteAddr() net.Addr       { return c.remoteAddr }
func (c *conn) Close() error {
	c.ErrClose(nil)
	return nil
}
func (c *conn) ErrClose(err error) {
	c.loopCloseConn(err)
}
func (c *conn) UpgradeTls(config *tls.Config) (err error) {
	c.tlsconn, err = tls.Server(c, &c.inboundBuffer, &c.outboundBuffer, config.Clone())
	c.readfd = func() error {
		n, err := unix.Read(c.fd, c.loop.packet)
		if n == 0 || err != nil {
			if err == unix.EAGAIN {
				return nil
			}
			c.loopCloseConn(err)
			return nil
		}
		c.tlsconn.RawWrite(c.loop.packet[:n])
		return nil
	}
	c.readframe = c.tlsread
	//很有可能握手包在UpgradeTls之前发过来了，这里把inboundBuffer剩余数据当做握手数据处理
	if c.inboundBuffer != nil && c.inboundBuffer.Len() > 0 {
		c.tlsconn.RawWrite(c.inboundBuffer.Bytes())
		c.inboundBuffer.Reset()
		if err := c.tlsconn.Handshake(); err != nil {
			return err
		}
	}
	//握手失败的关了
	time.AfterFunc(time.Second*5, func() {
		if c.state == connStateOk && (c.tlsconn == nil || !c.tlsconn.HandshakeComplete()) {
			c.Close()
		}
	})
	return err
}
func (c *conn) FlushWrite(data []byte, noCodec ...bool) {
	atomic.AddInt64(&c.flushWaitNum, 1)
	if len(noCodec) > 0 && noCodec[0] {
		c.WriteNoCodec(data)
	} else {
		c.AsyncWrite(data)
	}
out:
	for c.state == connStateOk {
		select {
		case buflen := <-c.flushWait:
			if buflen == 0 {
				break out
			}
		case <-time.After(time.Millisecond * 10):

			if c.state == connStateOk && (c.outboundBuffer == nil || c.outboundBuffer.Len() == 0) {
				break out
			}
		}
	}
	atomic.AddInt64(&c.flushWaitNum, -1)
}

// 在当前epoll事件中close
func (c *conn) loopCloseConn(err error) {
	if atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {

		c.loop.eventHandler.OnClosed(c, err)
		if c.tlsconn != nil { //关闭前通知tls关闭
			c.tlsconn.CloseWrite()
		}
		c.loop.lazyChan <- c
	}
	return
}

func (c *conn) lazywrite() {

	if c.state > connStateNone {
		for c.outboundBuffer.Len() > 0 {
			n, err := unix.Write(c.fd, c.outboundBuffer.PreBytes(sendbufDefault))
			if err != nil {
				if err == unix.EAGAIN {
					c.eagainNum++
					//判断是否超时
					if int((1+c.eagainNum)*c.eagainNum/2*delay/time.Second) > c.writetimeout {
						c.ErrClose(errors.New("write time out"))
						break
					}
					time.AfterFunc(delay*c.eagainNum, func() {
						c.loop.lazyChan <- c
					})
					break
				}
				c.Close()
				break
			} else {
				c.eagainNum = 0
			}
			c.outboundBuffer.Shift(n)
		}
		if atomic.CompareAndSwapInt32(&c.state, connStateCloseReady, connStateClosed) { //彻底删除close的c

			for i := c.flushWaitNum; i > 0; i-- {
				select {
				case c.flushWait <- 0:
				default:
				}
			}
			c.releaseTCP()
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
