//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"time"

	"github.com/luyu6056/gnet/pkg/pool/byteslice"
	"golang.org/x/sys/unix"
)

const (
	delay          = time.Millisecond
	sendbufDefault = 16384 //暂时设置为一个tls包大小吧
)

type out struct {
	conn *conn
	data []byte
}

func (lp *eventloop) loopOut() {
	bufnum := 128
	lp.outChan = make(chan out, bufnum)
	lp.lazyChan = make(chan *conn, bufnum) //产生了EAGAIN阻塞的连接
	lp.outclose = make(chan bool, 1)

	go func() { //单个gorutinue 减少unix.EAGAIN cpu消耗

		var c *conn
		for {
			c = nil
			select {
			case o := <-lp.outChan:
				writeOut(o)
			case c = <-lp.lazyChan:
			case <-lp.outclose:
				//循环至超时退出
				for {
					select {
					case o := <-lp.outChan:
						writeOut(o)
					case c := <-lp.lazyChan:
						lp.lazyChan <- c
					case <-time.After(time.Second):
						lp.outclose <- true
						return
					}
					for i := len(lp.outChan); i > 0; i-- {
						writeOut(<-lp.outChan)
					}
					for i := len(lp.lazyChan); i > 0; i-- {
						c := <-lp.lazyChan
						c.lazywrite()
					}
				}
			}

			//优先从不阻塞输出
			for i := len(lp.outChan); i > 0; i-- {

				writeOut(<-lp.outChan)
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

func writeOut(o out) {

	c, data := o.conn, o.data

	if c.state < connStateNone {

		for i := c.flushWaitNum; i > 0; i-- {
			select {
			case c.flushWait <- 0:
			default:
			}
		}
		byteslice.Put(data)
	} else {
		defer func() {
			for i := c.flushWaitNum; i > 0; i-- {
				select {
				case c.flushWait <- c.outboundBuffer.Len():
				default:
				}
			}
			byteslice.Put(data)
		}()
		if c.tlsconn != nil {
			c.tlsconn.Write(data) //这里已经写入outboundBuffer
			for c.outboundBuffer.Len() > 0 {
				n, err := unix.Write(c.fd, c.outboundBuffer.PreBytes(sendbufDefault))
				if err != nil {
					if err == unix.EAGAIN {
						c.eagainNum++
						time.AfterFunc(delay*c.eagainNum, func() {
							c.loop.lazyChan <- c
						})
						return
					}
					return
				}
				c.outboundBuffer.Shift(n)
			}
		} else {

			for c.outboundBuffer.Len() > 0 {
				n, err := unix.Write(c.fd, c.outboundBuffer.PreBytes(sendbufDefault))
				if err != nil {

					if err == unix.EAGAIN {
						c.eagainNum++
						time.AfterFunc(delay*c.eagainNum, func() {
							c.loop.lazyChan <- c
						})
						c.outboundBuffer.Write(data)
						return
					}
					return
				} else {
					c.eagainNum = 0
				}
				c.outboundBuffer.Shift(n)
			}

			for be, en := 0, sendbufDefault; be < len(data); en = be + sendbufDefault {
				if en > len(data) {
					en = len(data)
				}
				n, err := unix.Write(c.fd, data[be:en])
				if err != nil {
					if err == unix.EAGAIN {
						c.outboundBuffer.Write(data[be:])
						c.eagainNum++
						time.AfterFunc(delay*c.eagainNum, func() {
							c.loop.lazyChan <- c
						})
						return
					}
					return
				} else {
					c.eagainNum = 0
				}
				be += n
			}

		}

	}

}
