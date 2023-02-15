// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build linux
// +build linux

package gnet

func (svr *server) activateMainReactor() {
	defer svr.signalShutdown()

	sniffError(svr.mainLoop.startPolling(svr.activateMainReactorCallback))
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

	sniffError(lp.startPolling(func(fd int) error {

		if i, ok := lp.srv.connections.Load(fd); ok {
			c := i.(*conn)
			if c.state == connStateOk {
				return lp.loopIn(c)
			}
		}

		return nil
	}))
}
