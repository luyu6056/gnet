// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"golang.org/x/sys/unix"
)

func (svr *server) acceptNewConnection(fd int) error {
	nfd, sa, err := unix.Accept(fd)
	if err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		return err
	}
	if !svr.Isblock {
		if err := unix.SetNonblock(nfd, true); err != nil {
			return err
		}
	}
	lp := svr.subLoopGroup.getbyfd(nfd)
	c := newTCPConn(nfd, lp, sa)
	if svr.tlsconfig != nil {
		if err = c.UpgradeTls(svr.tlsconfig); err != nil {
			return err
		}
	}
	_ = lp.poller.Trigger(func() (err error) {
		if err = lp.poller.AddRead(nfd); err != nil {
			return
		}
		index := c.fd / lp.svr.subLoopGroup.len()
		if index >= len(lp.connections) {
			lp.connections = append(lp.connections, make([]*conn, len(lp.connections))...)
		}
		lp.connections[index] = c
		err = lp.loopOpen(c)
		return
	})
	return nil
}
