// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

func (lp *eventloop) handleEvent(fd int) error {

	if i, ok := lp.srv.connections.Load(fd); ok {
		c := i.(*conn)
		if c.state == connStateOk {
			return lp.loopIn(c)
		}

	}

	return lp.loopAccept(fd)
}

func (c *conn) handleEvents(fd int) error {

	if c.state == connStateOk {
		return c.loop.loopIn(c)
	}
	return nil
}
