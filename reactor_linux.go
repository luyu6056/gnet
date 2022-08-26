// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux

package gnet

func (svr *server) activateMainReactor() {
	defer svr.signalShutdown()

	sniffError(svr.mainLoop.poller.Polling(func(fd int, ev uint32) error {
		return svr.acceptNewConnection(fd)
	}))
}

func (svr *server) activateSubReactor(lp *eventloop) {
	defer func() {
		if lp.idx == 0 && svr.opts.Ticker {
			close(svr.ticktock)
		}
		svr.signalShutdown()
	}()

	if lp.idx == 0 && svr.opts.Ticker {
		go lp.loopTicker()
	}

	sniffError(lp.poller.Polling(func(fd int, ev uint32) error {
		if c := lp.connections[fd/lp.svr.subLoopGroup.len()]; c != nil && c.opened == connStateOk {
			return lp.loopIn(c)
		}
		return nil
	}))
}
