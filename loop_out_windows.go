//go:build windows
// +build windows

package gnet

import "github.com/luyu6056/gnet/pkg/pool/byteslice"

type out struct {
	conn *stdConn
	data []byte
}

func (el *eventloop) loopOut() {
	bufnum := 128
	el.outChan = make(chan out, bufnum)
	el.outclose = make(chan bool, 1)

	go func() {

		for {
			select {
			case o := <-el.outChan:
				c, data := o.conn, o.data
				if c.outboundBuffer != nil {
					if c.tlsconn != nil {
						c.tlsconn.Write(data)
						c.conn.Write(c.outboundBuffer.Bytes())
						c.outboundBuffer.Reset()
					} else {
						c.conn.Write(data)
					}
					for i := c.flushWaitNum; i > 0; i-- {
						select {
						case c.flushWait <- c.outboundBuffer.Len():
						default:
						}
					}

					if c.state == connStateCloseLazyout {
						c.state = connStateCloseOk
						c.ctx = nil
						c.localAddr = nil
						c.remoteAddr = nil
						c.inboundBuffer.Reset()
						c.outboundBuffer.Reset()
						msgbufpool.Put(c.outboundBuffer)
						msgbufpool.Put(c.inboundBuffer)
						c.inboundBuffer = nil
						c.outboundBuffer = nil
					}

				}

				byteslice.Put(data)
			case <-el.outclose:
				return
			}
		}
	}()
}
