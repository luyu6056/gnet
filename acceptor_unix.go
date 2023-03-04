// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
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
	if svr.opts.TCPNoDelay && svr.ln.addr == "tcp" {
		if err := unix.SetsockoptInt(nfd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {

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
	return lp.poller.Trigger(lp.addread, c)
}
