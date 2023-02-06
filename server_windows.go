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
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/luyu6056/tls"
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
	close            chan error
}

// waitForShutdown waits for a signal to shutdown.
func (srv *server) waitForShutdown() error {
	srv.cond.L.Lock()
	srv.cond.Wait()
	err := srv.serr
	srv.cond.L.Unlock()
	return err
}

// signalShutdown signals a shutdown an begins server closing.
func (srv *server) signalShutdown(err error) {
	srv.once.Do(func() {

		srv.cond.L.Lock()
		srv.serr = err
		srv.cond.Signal()
		srv.cond.L.Unlock()
	})
	os.Exit(1)
}

func (srv *server) startListener() {
	srv.listenerWG.Add(1)
	go func() {
		srv.listenerRun()
		srv.listenerWG.Done()
	}()
}

func (srv *server) startLoops(numLoops int) {
	for i := 0; i < numLoops; i++ {
		el := &eventloop{
			ch:           make(chan interface{}, commandBufferSize),
			idx:          i,
			srv:          srv,
			codec:        srv.codec,
			connections:  make(map[*stdConn]bool),
			eventHandler: srv.eventHandler,
		}
		srv.subLoopGroup.register(el)
	}

	srv.subLoopGroupSize = srv.subLoopGroup.len()
	srv.loopWG.Add(srv.subLoopGroupSize)
	srv.subLoopGroup.iterate(func(i int, el *eventloop) bool {
		go el.loopRun()
		go el.loopOut()
		return true
	})
}

func (srv *server) stop() {

	// Wait on a signal for shutdown.
	log.Printf("server is being shutdown with err: %v\n", srv.waitForShutdown())
	if srv.ln != nil {
		// Close listener.
		srv.ln.close()
		srv.listenerWG.Wait()

	}

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

	srv := new(server)
	srv.close = make(chan error, numCPU+1)
	srv.opts = options
	srv.tlsconfig = options.Tlsconfig
	srv.eventHandler = eventHandler
	srv.ln = &ln
	srv.subLoopGroup = new(eventLoopGroup)
	srv.ticktock = make(chan time.Duration, 1)
	srv.cond = sync.NewCond(&sync.Mutex{})
	srv.codec = func() ICodec {
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
		Close: func() {
			srv.close <- errors.New("close by server.Close()")
		},
	}

	switch srv.eventHandler.OnInitComplete(server) {
	case None:
	case Shutdown:
		return
	}
	go srv.signalHandler()
	// Start all loops.
	srv.startLoops(numCPU)
	// Start listener.
	srv.startListener()
	srv.stop()
	return
}
func (srv *server) signalHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case sig := <-ch:
			signal.Stop(ch)
			// Notify all loops to close.
			srv.subLoopGroup.iterate(func(i int, el *eventloop) bool {
				el.ch <- errClosing
				el.outclose <- true
				return true
			})

			// Wait on all loops to close.
			srv.loopWG.Wait()

			// Close all connections.
			srv.loopWG.Add(srv.subLoopGroupSize)
			srv.subLoopGroup.iterate(func(i int, el *eventloop) bool {
				el.ch <- errCloseConns
				return true
			})
			srv.loopWG.Wait()
			// timeout context for shutdown
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				srv.signalShutdown(nil)
				return

			}
		case err := <-srv.close:
			signal.Stop(ch)
			srv.signalShutdown(err)
			return
		}
	}

}
