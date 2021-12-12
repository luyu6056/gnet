// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luyu6056/gnet/internal/netpoll"
	"github.com/luyu6056/tls"
	"golang.org/x/sys/unix"
)

const (
	connStateOk           = 1
	connStateCloseReady   = -0
	connStateCloseLazyout = -1
	connStateCloseOk      = -2
)

var msgbufpool = sync.Pool{New: func() interface{} {
	return &tls.MsgBuffer{}
}}

type conn struct {
	fd                 int            // file descriptor
	sa                 unix.Sockaddr  // remote socket address
	ctx                interface{}    // user-defined context
	loop               *eventloop     // connected loop
	codec              ICodec         // codec for TCP
	state              int32          // connection opened event fired
	localAddr          net.Addr       // local addr
	remoteAddr         net.Addr       // remote addr
	inboundBuffer      *tls.MsgBuffer // buffer for data from client
	outboundBuffer     *tls.MsgBuffer
	tlsconn            *tls.Conn
	inboundBufferWrite func([]byte) (int, error)
	readframe          func() []byte
	eagainNum          time.Duration
	flushWaitNum       int64
	flushWait          chan int
}

func newTCPConn(fd int, lp *eventloop, sa unix.Sockaddr) *conn {
	c := &conn{
		fd:             fd,
		sa:             sa,
		loop:           lp,
		codec:          lp.codec,
		inboundBuffer:  msgbufpool.Get().(*tls.MsgBuffer),
		outboundBuffer: msgbufpool.Get().(*tls.MsgBuffer),
		flushWait:      make(chan int),
	}
	c.inboundBufferWrite = c.inboundBuffer.Write
	c.readframe = c.read
	c.inboundBuffer.Reset()
	c.outboundBuffer.Reset()
	return c
}

func (c *conn) releaseTCP() {
	c.sa = nil
	c.ctx = nil
	c.inboundBuffer.Reset()
	c.outboundBuffer.Reset()
	msgbufpool.Put(c.inboundBuffer)
	msgbufpool.Put(c.outboundBuffer)
	c.inboundBuffer = nil
	c.outboundBuffer = nil
}

var conn_m sync.Map

func newUDPConn(fd int, lp *eventloop, sa unix.Sockaddr) (c *conn) {
	if v, ok := conn_m.Load(netpoll.SockaddrToUDPAddr(sa).String()); ok {
		c = v.(*conn)
	} else {
		c = &conn{
			localAddr:  lp.svr.ln.lnaddr,
			remoteAddr: netpoll.SockaddrToUDPAddr(sa),
		}
	}
	c.fd = fd
	c.sa = sa
	c.loop = lp
	return
}

func (c *conn) releaseUDP() {

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
			if err != nil && atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {
				_ = c.loop.poller.Trigger(func() error {
					return c.loop.loopCloseConn(c, err)
				})
				return
			}
		}
		if !c.tlsconn.HandshakeComplete() || len(c.tlsconn.RawData()) == 0 { //握手没成功，或者握手成功，但是没有数据黏包了
			return
		}
	}

	for err = c.tlsconn.ReadFrame(); err == nil; err = c.tlsconn.ReadFrame() { //循环读取直到获得
		frame, err = c.codec.Decode(c)
		if err != nil && atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {
			_ = c.loop.poller.Trigger(func() error {
				return c.loop.loopCloseConn(c, err)
			})
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
	if err != nil && atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {
		_ = c.loop.poller.Trigger(func() error {
			return c.loop.loopCloseConn(c, err)
		})
	}
	return frame
}
func (c *conn) write(buf []byte) {
	o := <-c.loop.outbufchan
	o.c = c
	o.b.Write(buf)
	c.loop.outChan <- o
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

//用于直出不编码的出口，tls调用
func (c *conn) Write(buf []byte) (n int, err error) {
	o := <-c.loop.outbufchan
	o.c = c
	o.b.Write(buf)
	c.loop.outChan <- o
	return len(buf), nil
}

func (c *conn) AsyncWrite(buf []byte) error {
	encodedBuf, err := c.codec.Encode(c, buf)
	if len(encodedBuf) > 0 {
		o := <-c.loop.outbufchan
		o.c = c
		o.b.Write(buf)
		c.loop.outChan <- o
	} else if err != nil && atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {
		_ = c.loop.poller.Trigger(func() error {
			return c.loop.loopCloseConn(c, err)

		})
	}
	return err
}
func (c *conn) WriteNoCodec(buf []byte) error {
	o := <-c.loop.outbufchan
	o.c = c
	o.b.Write(buf)
	c.loop.outChan <- o
	return nil
}
func (c *conn) SendTo(buf []byte) error {
	return unix.Sendto(c.fd, buf, 0, c.sa)
}

func (c *conn) Wake() error {
	return c.loop.poller.Trigger(func() error {
		return c.loop.loopWake(c)
	})

}

func (c *conn) Context() interface{}       { return c.ctx }
func (c *conn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *conn) LocalAddr() net.Addr        { return c.localAddr }
func (c *conn) RemoteAddr() net.Addr       { return c.remoteAddr }
func (c *conn) Close() error {

	if atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {
		_ = c.loop.poller.Trigger(func() error {
			return c.loop.loopCloseConn(c, nil)
		})
	}
	return nil
}
func (c *conn) UpgradeTls(config *tls.Config) (err error) {
	c.tlsconn, err = tls.Server(c, c.inboundBuffer, c.outboundBuffer, config.Clone())
	c.inboundBufferWrite = c.tlsconn.RawWrite
	c.readframe = c.tlsread
	//很有可能握手包在UpgradeTls之前发过来了，这里把inboundBuffer剩余数据当做握手数据处理
	if c.inboundBuffer.Len() > 0 {
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
