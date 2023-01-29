// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet


func (lp *eventloop) handleEvent(fd int) error {
	if fd < len(lp.svr.connections) {
		if c := lp.svr.connections[fd]; c != nil && c.state == connStateOk {
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
