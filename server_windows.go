// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build windows

package gnet

import (
	"errors"
	"log"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/luyu6056/gnet/tls"
)

// commandBufferSize represents the buffer size of event-loop command channel on Windows.
const (
	commandBufferSize = 512
)

var (
	errClosing    = errors.New("closing")
	errCloseConns = errors.New("close conns")
)

type server struct {
	ln               *listener          // all the listeners
	cond             *sync.Cond         // shutdown signaler
	opts             *Options           // options with server
	serr             error              // signal error
	once             sync.Once          // make sure only signalShutdown once
	codec            ICodec             // codec for TCP stream
	loops            []*eventloop       // all the loops
	loopWG           sync.WaitGroup     // loop close WaitGroup
	ticktock         chan time.Duration // ticker channel
	listenerWG       sync.WaitGroup     // listener close WaitGroup
	eventHandler     EventHandler       // user eventHandler
	subLoopGroup     IEventLoopGroup    // loops for handling events
	subLoopGroupSize int                // number of loops
	tlsconfig        *tls.Config
}

// waitForShutdown waits for a signal to shutdown.
func (svr *server) waitForShutdown() error {
	svr.cond.L.Lock()
	svr.cond.Wait()
	err := svr.serr
	svr.cond.L.Unlock()
	return err
}

// signalShutdown signals a shutdown an begins server closing.
func (svr *server) signalShutdown(err error) {
	svr.once.Do(func() {
		svr.cond.L.Lock()
		svr.serr = err
		svr.cond.Signal()
		svr.cond.L.Unlock()
	})
}

func (svr *server) startListener() {
	svr.listenerWG.Add(1)
	go func() {
		svr.listenerRun()
		svr.listenerWG.Done()
	}()
}

func (svr *server) startLoops(numLoops int) {
	for i := 0; i < numLoops; i++ {
		el := &eventloop{
			ch:           make(chan interface{}, commandBufferSize),
			idx:          i,
			svr:          svr,
			codec:        svr.codec,
			connections:  make(map[*stdConn]bool),
			eventHandler: svr.eventHandler,
		}
		svr.subLoopGroup.register(el)
	}
	svr.subLoopGroupSize = svr.subLoopGroup.len()
	svr.loopWG.Add(svr.subLoopGroupSize)
	svr.subLoopGroup.iterate(func(i int, el *eventloop) bool {
		go el.loopRun()
		go el.loopOut(svr.opts.OutbufNum)
		return true
	})
}

func (svr *server) stop() {
	// Wait on a signal for shutdown.
	log.Printf("server is being shutdown with err: %v\n", svr.waitForShutdown())

	// Close listener.
	svr.ln.close()
	svr.listenerWG.Wait()

	// Notify all loops to close.
	svr.subLoopGroup.iterate(func(i int, el *eventloop) bool {
		el.ch <- errClosing
		el.outclose <- true
		return true
	})

	// Wait on all loops to close.
	svr.loopWG.Wait()

	// Close all connections.
	svr.loopWG.Add(svr.subLoopGroupSize)
	svr.subLoopGroup.iterate(func(i int, el *eventloop) bool {
		el.ch <- errCloseConns
		return true
	})
	svr.loopWG.Wait()
	return
}

func serve(eventHandler EventHandler, addr string, options *Options) (err error) {
	var ln listener
	defer ln.close()

	ln.network, ln.addr = parseAddr(addr)
	if ln.network == "unix" {
		sniffError(os.RemoveAll(ln.addr))
	}

	if ln.network == "udp" {
		ln.pconn, err = net.ListenPacket(ln.network, ln.addr)
	} else {
		ln.ln, err = net.Listen(ln.network, ln.addr)
	}
	if err != nil {
		return err
	}
	if ln.pconn != nil {
		ln.lnaddr = ln.pconn.LocalAddr()
	} else {
		ln.lnaddr = ln.ln.Addr()
	}
	if err := ln.system(); err != nil {
		return err
	}
	// Figure out the correct number of loops/goroutines to use.
	numCPU := options.LoopNum
	if numCPU <= 0 {
		numCPU = runtime.NumCPU()
	}

	svr := new(server)
	svr.opts = options
	svr.tlsconfig = options.Tlsconfig
	svr.eventHandler = eventHandler
	svr.ln = &ln
	svr.subLoopGroup = new(eventLoopGroup)
	svr.ticktock = make(chan time.Duration, 1)
	svr.cond = sync.NewCond(&sync.Mutex{})
	svr.codec = func() ICodec {
		if options.Codec == nil {
			return new(BuiltInFrameCodec)
		}
		return options.Codec
	}()

	server := Server{
		Multicore:    numCPU > 1,
		Addr:         ln.lnaddr,
		NumEventLoop: numCPU,
		ReUsePort:    options.ReusePort,
		TCPKeepAlive: options.TCPKeepAlive,
	}
	switch svr.eventHandler.OnInitComplete(server) {
	case None:
	case Shutdown:
		return
	}

	// Start all loops.
	svr.startLoops(numCPU)
	// Start listener.
	svr.startListener()
	defer svr.stop()

	return
}
