// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly

package gnet

func (lp *eventloop) handleEvent(fd int, filter int16) error {
	index := fd / lp.svr.subLoopGroup.len()
	if index < len(lp.connections) {
		if c := lp.connections[index]; c != nil && c.opened == connStateOk {
			return lp.loopIn(c)
		}
	}
	return lp.loopAccept(fd)
}
