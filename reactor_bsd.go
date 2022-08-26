// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly

package gnet

func (svr *server) activateMainReactor() {
	defer svr.signalShutdown()

	sniffError(svr.mainLoop.poller.Polling(func(fd int, filter int16) error {
		return svr.acceptNewConnection(fd)
	}))
}

func (svr *server) activateSubReactor(lp *eventloop) {
	defer svr.signalShutdown()

	if lp.idx == 0 && svr.opts.Ticker {
		go lp.loopTicker()
	}

	sniffError(lp.poller.Polling(func(fd int, filter int16) error {
		if c := lp.connections[fd/lp.svr.subLoopGroup.len()]; c != nil && c.state == connStateOk {
			return lp.loopIn(c)
		}
		return nil
	}))
}
