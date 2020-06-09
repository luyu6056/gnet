// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build windows

package gnet

import (
	"net"
	"sync"

	"github.com/luyu6056/gnet/tls"
)

var msgbufpool = sync.Pool{New: func() interface{} {
	return &tls.MsgBuffer{}
}}

type stderr struct {
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
	done                          int32          // 0: attached, 1: closed
	codec                         ICodec         // codec for TCP
	localAddr                     net.Addr       // local server addr
	remoteAddr                    net.Addr       // remote peer addr
	inboundBuffer, outboundBuffer *tls.MsgBuffer // buffer for data from client
	tlsconn                       *tls.Conn
	inboundBufferWrite            func([]byte) (int, error)
	readframe                     func() []byte
}

func newTCPConn(conn net.Conn, lp *eventloop) *stdConn {
	c := &stdConn{
		conn:           conn,
		loop:           lp,
		codec:          lp.codec,
		inboundBuffer:  msgbufpool.Get().(*tls.MsgBuffer),
		outboundBuffer: msgbufpool.Get().(*tls.MsgBuffer),
	}
	c.inboundBufferWrite = c.inboundBuffer.Write
	c.readframe = c.read
	return c
}

func (c *stdConn) releaseTCP() {
	c.ctx = nil
	c.localAddr = nil
	c.remoteAddr = nil
	c.inboundBuffer.Reset()
	c.outboundBuffer.Reset()
	msgbufpool.Put(c.outboundBuffer)
	msgbufpool.Put(c.inboundBuffer)
	c.inboundBuffer = nil
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
	c.inboundBuffer.Reset()
	msgbufpool.Put(c.inboundBuffer)
	c.inboundBuffer = nil
}
func (c *stdConn) tlsread() (frame []byte) {
	var err error
	if !c.tlsconn.HandshakeComplete() {
		//先判断是否足够一条消息
		data := c.tlsconn.RawData()
		if len(data) < 5 || len(data) < 5+int(data[3])<<8|int(data[4]) {
			return nil
		}
		if err := c.tlsconn.Handshake(); err != nil || len(c.tlsconn.RawData()) == 0 {
			if err != nil {
				c.Close()
			}
			return nil
		}
	}
	for err = c.tlsconn.ReadFrame(); err == nil; err = c.tlsconn.ReadFrame() { //循环读取直到获得
		frame, err = c.codec.Decode(c)
		if err != nil {
			c.Close()
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
		c.Close()
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
		o := <-c.loop.outbufchan
		o.b.Write(encodedBuf)
		o.c = c
		c.loop.outChan <- o
	} else {
		c.Close()
	}
	return nil
}
func (c *stdConn) Write(buf []byte) (int, error) {
	if encodedBuf, err := c.codec.Encode(c, buf); err == nil {
		o := <-c.loop.outbufchan
		o.b.Write(encodedBuf)
		o.c = c
		c.loop.outChan <- o
	} else {
		c.Close()
	}
	return len(buf), nil
}
func (c *stdConn) SendTo(buf []byte) (err error) {
	_, err = c.loop.svr.ln.pconn.WriteTo(buf, c.remoteAddr)
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
		c.loop.loopClose(c)
		return nil
	}
	return nil
}
func (c *stdConn) UpgradeTls(config *tls.Config) (err error) {
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
	return err
}
