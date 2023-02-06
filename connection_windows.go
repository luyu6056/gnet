// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build windows
// +build windows

package gnet

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luyu6056/gnet/pkg/pool/byteslice"
	"github.com/luyu6056/tls"
)

const (
	connStateOk           = 1
	connStateCloseReady   = -0
	connStateCloseLazyout = -1
	connStateCloseOk      = -2
)

type stderr struct {
	c   *stdConn
	err error
}
type tcpClose struct {
	c   *stdConn
	err error
}
type wakeReq struct {
	c *stdConn
}

type tcpIn struct {
	c   *stdConn
	buf *tls.MsgBuffer
}

type udpIn struct {
	c *stdConn
}

type stdConn struct {
	ctx                           interface{}    // user-defined context
	conn                          net.Conn       // original connection
	loop                          *eventloop     // owner loop
	state                         int32          // connState
	codec                         ICodec         // codec for TCP
	localAddr                     net.Addr       // local server addr
	remoteAddr                    net.Addr       // remote peer addr
	inboundBuffer, outboundBuffer *tls.MsgBuffer // buffer for data from client
	tlsconn                       *tls.Conn
	inboundBufferWrite            func([]byte) (int, error)
	readframe                     func() []byte
	flushWait                     chan int
	flushWaitNum                  int64
	writetimeout   int
}

var msgbufpool = sync.Pool{New: func() interface{} {
	return &tls.MsgBuffer{}
}}

func newTCPConn(conn net.Conn, lp *eventloop) *stdConn {
	c := &stdConn{
		conn:           conn,
		loop:           lp,
		codec:          lp.codec,
		localAddr:      conn.LocalAddr(),
		remoteAddr:     conn.RemoteAddr(),
		inboundBuffer:  nil,
		outboundBuffer: msgbufpool.Get().(*tls.MsgBuffer),
		flushWait:      make(chan int),
		state:          connStateOk,
		writetimeout: lp.srv.opts.WriteTimeOut,
	}
	c.inboundBufferWrite =  func(b []byte) (int, error){
		return c.inboundBuffer.Write(b)
	}
	c.readframe = c.read
	return c
}

func (c *stdConn) releaseTCP() {
	c.loop.outChan <- out{c, byteslice.Get(0)} //修改至outchan进行回收
}

func newUDPConn(lp *eventloop, localAddr, remoteAddr net.Addr) *stdConn {
	return &stdConn{
		loop:          lp,
		localAddr:     localAddr,
		remoteAddr:    remoteAddr,
		inboundBuffer: msgbufpool.Get().(*tls.MsgBuffer),
	}
}

func (c *stdConn) releaseUDP() {
	c.ctx = nil
	c.localAddr = nil
	if c.inboundBuffer!=nil{
		c.inboundBuffer.Reset()
		msgbufpool.Put(c.inboundBuffer)
		c.inboundBuffer = nil
	}


}
func (c *stdConn) tlsread() (frame []byte) {
	var err error

	if !c.tlsconn.HandshakeComplete() {

		//先判断是否足够一条消息
		data := c.tlsconn.RawData()
		if len(data) < 5 || len(data) < 5+int(data[3])<<8|int(data[4]) {
			return nil
		}
		if err = c.tlsconn.Handshake(); err != nil || len(c.tlsconn.RawData()) == 0 {

			if err != nil {
				c.loop.loopError(c, err)
			}
			return nil
		}
	}

	for err = c.tlsconn.ReadFrame(); err == nil; err = c.tlsconn.ReadFrame() { //循环读取直到获得
		frame, err = c.codec.Decode(c)
		if err != nil {
			c.loop.loopError(c, err)
		}
		if frame != nil {
			return frame
		}
	}
	return nil
}
func (c *stdConn) read() []byte {
	frame, err := c.codec.Decode(c)
	if err != nil {
		c.loop.ch <- func() error {
			c.loop.loopError(c, err)
			return nil
		}
		return nil
	}
	return frame
}

// ================================= Public APIs of gnet.Conn =================================

func (c *stdConn) Read() []byte {
	return c.inboundBuffer.Bytes()
}

func (c *stdConn) ResetBuffer() {
	c.inboundBuffer.Reset()
}

func (c *stdConn) ShiftN(n int) (size int) {
	c.inboundBuffer.Shift(n)
	return n
}

func (c *stdConn) ReadN(n int) (size int, buf []byte) {
	buf = c.inboundBuffer.PreBytes(n)
	size = len(buf)
	return
}

func (c *stdConn) BufferLength() int {
	return c.inboundBuffer.Len()
}
func (c *stdConn) OutBufferLength() int {
	return 0
}
func (c *stdConn) AsyncWrite(buf []byte) error {
	if encodedBuf, err := c.codec.Encode(c, buf); err == nil {
		if len(encodedBuf) > 0 {
			c.Write(encodedBuf)
		}

	} else {
		c.loop.ch <- func() error {
			c.loop.loopError(c, err)
			return nil
		}
	}
	return nil
}

//用于直出不编码的出口，tls调用
func (c *stdConn) Write(buf []byte) (n int, err error) {
	data := byteslice.Get(len(buf))
	copy(data, buf)
	c.loop.outChan <- out{c, data}
	return len(buf), nil
}

func (c *stdConn) WriteNoCodec(buf []byte) error {
	c.Write(buf)
	return nil
}
func (c *stdConn) SendTo(buf []byte) (err error) {
	_, err = c.loop.srv.ln.pconn.WriteTo(buf, c.remoteAddr)
	return err
}

func (c *stdConn) Context() interface{}       { return c.ctx }
func (c *stdConn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *stdConn) LocalAddr() net.Addr        { return c.localAddr }
func (c *stdConn) RemoteAddr() net.Addr       { return c.remoteAddr }
func (c *stdConn) Wake() error {
	c.loop.ch <- wakeReq{c}
	return nil
}
func (c *stdConn) Close() error {

	c.loop.ch <- func() error {
		c.state = connStateCloseReady
		c.loop.loopClose(c)
		return nil
	}
	return nil
}
func (c *stdConn) UpgradeTls(config *tls.Config) (err error) {
	c.loop.ch <- func() error {

		c.tlsconn, err = tls.Server(c, &c.inboundBuffer, &c.outboundBuffer, config.Clone())
		c.inboundBufferWrite = c.tlsconn.RawWrite
		c.readframe = c.tlsread
		//很有可能握手包在UpgradeTls之前发过来了，这里把inboundBuffer剩余数据当做握手数据处理
		if c.inboundBuffer!=nil && c.inboundBuffer.Len() > 0 {
			c.tlsconn.RawWrite(c.inboundBuffer.Bytes())
			c.inboundBuffer.Reset()
			if err := c.tlsconn.Handshake(); err != nil {
				return err
			}
		}
		return err
	}

	return err
}

func (c *stdConn) FlushWrite(data []byte, noCodec ...bool) {
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
func (c *stdConn)SetWriteDeadline(i int64)  {
	c.conn.SetWriteDeadline(time.Unix(i,0))
}