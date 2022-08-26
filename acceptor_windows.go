// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build windows
// +build windows

package gnet

import (
	"github.com/luyu6056/tls"
	"time"
)

func (svr *server) listenerRun() {
	var err error
	defer func() { svr.signalShutdown(err) }()
	var packet [0x10000]byte
	for {
		if svr.ln.pconn != nil {
			// Read data from UDP socket.
			n, addr, e := svr.ln.pconn.ReadFrom(packet[:])
			if e != nil {
				err = e
				return
			}
			el := svr.subLoopGroup.next()
			c := newUDPConn(el, svr.ln.lnaddr, addr)
			c.inboundBuffer.Write(packet[:n])
			el.ch <- &udpIn{c}
		} else {

			// Accept TCP socket.
			conn, e := svr.ln.ln.Accept()
			if e != nil {
				err = e
				return
			}
			el := svr.subLoopGroup.next()
			c := newTCPConn(conn, el)
			if svr.tlsconfig != nil {
				if err = c.UpgradeTls(svr.tlsconfig); err != nil {
					return
				}
			}

			el.loopAccept(c)
			go func() {
				var packet = make([]byte, 2000)
				for {
					n, err := c.conn.Read(packet)
					if err != nil {
						_ = c.conn.SetReadDeadline(time.Time{})
						el.ch <- tcpClose{c, err}
						return
					}

					msg := msgbufpool.Get().(*tls.MsgBuffer)
					msg.Write(packet[:n])
					el.ch <- &tcpIn{c, msg}
				}
			}()
		}
	}
}
