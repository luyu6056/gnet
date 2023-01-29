// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"flag"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/luyu6056/gnet/pkg/errors"

	"github.com/luyu6056/gnet/internal/netpoll"
	"github.com/luyu6056/tls"
	"golang.org/x/sys/unix"
)

type server struct {
	ln               *listener          // all the listeners
	wg               sync.WaitGroup     // loop close WaitGroup
	opts             *Options           // options with server
	once             sync.Once          // make sure only signalShutdown once
	cond             *sync.Cond         // shutdown signaler
	codec            ICodec             // codec for TCP stream
	ticktock         chan time.Duration // ticker channel
	mainLoop         *eventloop         // main loop for accepting connections
	eventHandler     EventHandler       // user eventHandler
	subLoopGroup     IEventLoopGroup    // loops for handling events
	subLoopGroupSize int                // number of loops
	Isblock          bool               //允许阻塞
	tlsconfig        *tls.Config
	close            chan bool
	connections      []*conn // loop connections fd -> conn
	connectionsLock  sync.Mutex
}

// waitForShutdown waits for a signal to shutdown
func (svr *server) waitForShutdown() {
	svr.cond.L.Lock()
	svr.cond.Wait()
	svr.cond.L.Unlock()
	svr.stop()
}

// signalShutdown signals a shutdown an begins server closing
func (svr *server) signalShutdown() {
	svr.once.Do(func() {
		svr.cond.L.Lock()
		svr.cond.Signal()
		svr.cond.L.Unlock()
	})
}

func (svr *server) startLoops() {
	svr.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
		svr.wg.Add(1)
		go func() {
			lp.loopOut()
			lp.loopRun()
			svr.wg.Done()
		}()
		return true
	})
}
func (svr *server) closeConns(_ interface{}) error {
	for _, c := range svr.connections {
		if c != nil {
			if atomic.CompareAndSwapInt32(&c.state, connStateOk, connStateCloseReady) {
				sniffError(c.loopCloseConn(errors.ErrEngineShutdown))
			}

		}
	}
	return nil
}
func (svr *server) closeLoops() {

	sniffError(svr.mainLoop.poller.Trigger(svr.closeConns, nil))
	svr.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
		lp.outclose <- true
		<-lp.outclose
		lp.poller.Close()
		return true
	})

}

func (svr *server) startReactors() {
	svr.subLoopGroup.iterate(func(i int, el *eventloop) bool {
		svr.wg.Add(1)
		go func() {
			el.loopOut()
			svr.activateSubReactor(el)
			svr.wg.Done()
		}()
		return true
	})
}

func (svr *server) activateLoops(numLoops int) error {
	// Create loops locally and bind the listeners.

	for i := 0; i < numLoops; i++ {
		if p, err := netpoll.OpenPoller(); err == nil {
			el := &eventloop{
				idx:          i,
				svr:          svr,
				codec:        svr.codec,
				poller:       p,
				packet:       make([]byte, 0xFFFF),
				eventHandler: svr.eventHandler,
			}

			el.pollAttachment = netpoll.GetPollAttachment()
			el.pollAttachment.FD = svr.ln.fd
			el.pollAttachment.Callback = el.handleEvent
			_ = el.poller.AddRead(el.pollAttachment)
			svr.subLoopGroup.register(el)
		} else {
			return err
		}
	}

	svr.subLoopGroupSize = svr.subLoopGroup.len()
	// Start loops in background
	svr.startLoops()
	return nil
}

func (svr *server) activateReactors(numLoops int) error {
	if p, err := netpoll.OpenPoller(); err == nil {
		el := &eventloop{
			idx:    -1,
			poller: p,
			svr:    svr,
		}
		el.pollAttachment = netpoll.GetPollAttachment()
		el.pollAttachment.FD = svr.ln.fd
		el.pollAttachment.Callback = svr.activateMainReactorCallback
		_ = el.poller.AddRead(el.pollAttachment)
		svr.mainLoop = el
		// Start main reactor.
		svr.wg.Add(1)
		go func() {

			svr.activateMainReactor()
			svr.wg.Done()
		}()
	} else {
		return err
	}
	for i := 0; i < numLoops; i++ {
		if p, err := netpoll.OpenPoller(); err == nil {
			el := &eventloop{
				idx:          i,
				svr:          svr,
				codec:        svr.codec,
				poller:       p,
				packet:       make([]byte, 0xFFFF),
				eventHandler: svr.eventHandler,
			}

			svr.subLoopGroup.register(el)
		} else {
			return err
		}
	}
	svr.subLoopGroupSize = svr.subLoopGroup.len()
	// Start sub reactors.
	svr.startReactors()

	return nil
}

func (svr *server) activateMainReactorCallback(fd int) error {
	return svr.acceptNewConnection(fd)
}

func (svr *server) start(numCPU int) error {
	if svr.opts.ReusePort || svr.ln.pconn != nil {
		return svr.activateLoops(numCPU)
	}
	return svr.activateReactors(numCPU)
}

func (svr *server) stop() {

	// Close loops and all outstanding connections
	sniffError(svr.mainLoop.poller.Trigger(svr.closeConns, nil))

	// Wait on all loops to complete reading events

	// Notify all loops to close by closing all listeners
	svr.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
		sniffError(lp.poller.Trigger(func(_ interface{}) error {
			return errors.ErrEngineShutdown
		}, nil))
		return true
	})
	svr.closeLoops()
	if svr.mainLoop != nil {
		sniffError(svr.mainLoop.poller.Trigger(func(_ interface{}) error {
			return errors.ErrEngineShutdown
		}, nil))
	}
	svr.wg.Wait()

	if svr.mainLoop != nil {
		sniffError(svr.mainLoop.poller.Close())
		svr.mainLoop.outclose <- true
		<-svr.mainLoop.outclose
	}
}

//tcp平滑重启，开启ReusePort有效，关闭ReusePort则会造成短暂的错误
var (
	reload, graceful, stop *bool
)

func serve(eventHandler EventHandler, addr string, options *Options) error {
	svr := new(server)
	var ln listener
	//efer ln.close()

	ln.network, ln.addr = parseAddr(addr)
	if ln.network == "unix" {
		sniffError(os.RemoveAll(ln.addr))
	}
	var err error

	if ln.network == "udp" {
		ln.pconn, err = net.ListenPacket(ln.network, ln.addr)
	} else {
		flag.Parse()
		if stop != nil && *stop {
			b, err := ioutil.ReadFile("./pid")
			if err == nil {
				pidstr := string(b)
				pid, err := strconv.Atoi(pidstr)
				if err == nil {
					if err = syscall.Kill(pid, syscall.SIGTERM); err == nil {
						log.Println("stop server ok")
						return nil
					}
				}
			}
			log.Println("stop server fail or server not start")
			return nil
		}
		if reload != nil && *reload {
			b, err := ioutil.ReadFile("./pid")
			if err == nil {
				pidstr := string(b)
				pid, err := strconv.Atoi(pidstr)
				if err == nil {
					if err = syscall.Kill(pid, syscall.SIGUSR1); err == nil {
						log.Println("reload ok")
						return nil
					}
				}
			}
		}
		if graceful != nil && *graceful {
			f := os.NewFile(3, "")
			ln.ln, err = net.FileListener(f)
		} else {
			ln.ln, err = net.Listen(ln.network, ln.addr)
		}

		if err == nil {
			pid := unix.Getpid()
			f, err := os.OpenFile("./pid", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
			if err != nil {
				return err
			}
			f.WriteString(strconv.Itoa(int(pid)))
			f.Close()
			go svr.signalHandler()
		}

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
	svr.close = make(chan bool, 1)

	svr.opts = options
	svr.tlsconfig = options.Tlsconfig
	svr.eventHandler = eventHandler
	svr.ln = &ln
	svr.subLoopGroup = new(eventLoopGroup)
	svr.cond = sync.NewCond(&sync.Mutex{})
	svr.ticktock = make(chan time.Duration, 1)
	svr.Isblock = options.Isblock
	svr.codec = func() ICodec {
		if options.Codec == nil {
			return new(BuiltInFrameCodec)
		}
		return options.Codec
	}()

	svr.connections = make([]*conn, 256)
	server := Server{
		Multicore:    numCPU > 1,
		Addr:         ln.lnaddr,
		NumEventLoop: numCPU,
		ReUsePort:    options.ReusePort,
		TCPKeepAlive: options.TCPKeepAlive,
		Close: func() {
			svr.close <- true
		},
	}
	if svr.opts.ReusePort {
		err := unix.SetsockoptInt(svr.ln.fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err != nil {
			return err
		}

		err = unix.SetsockoptInt(svr.ln.fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		if err != nil {
			return err
		}
	}
	switch svr.eventHandler.OnInitComplete(server) {
	case None:
	case Shutdown:
		return nil
	}

	if err := svr.start(numCPU); err != nil {
		svr.closeLoops()
		log.Printf("gnet server is stoping with error: %v\n", err)
		return err
	}

	svr.waitForShutdown()

	return nil
}
func (svr *server) signalHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	select {
	case sig := <-ch:
		signal.Stop(ch)
		var wg, wg1 sync.WaitGroup
		wg.Add(svr.subLoopGroup.len())
		wg1.Add(1)
		svr.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
			sniffError(lp.poller.Trigger(func(_ interface{}) error {
				wg.Done()
				wg1.Wait()
				return nil
			}, nil))
			return true
		})
		wg.Wait()
		svr.ln.fd = 0 // 修改监听fd让accept失效
		wg1.Done()
		// timeout context for shutdown
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			// stop
			log.Println("signal: stop")
			svr.signalShutdown()
			syscall.Kill(unix.Getpid(), syscall.SIGTERM)
			return
		case syscall.SIGUSR1:
			if svr.ln != nil {
				// reload
				log.Println("signal: reload")
				f, err := svr.ln.ln.(*net.TCPListener).File()
				var args []string
				if err == nil {
					args = []string{"-graceful"}
				}
				cmd := exec.Command(os.Args[0], args...)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				// put socket FD at the first entry
				cmd.ExtraFiles = []*os.File{f}
				cmd.Start()
				svr.signalShutdown()
			}

			return
		}
	case <-svr.close:
		log.Println("close gnet")
		svr.close <- true
		svr.signalShutdown()
		syscall.Kill(unix.Getpid(), syscall.SIGTERM)
		return
	}

}
func init() {
	defer func() {
		recover()
	}()
	reload = flag.Bool("reload", false, "listen on fd open 3 (internal use only)")
	graceful = flag.Bool("graceful", false, "listen on fd open 3 (internal use only)")
	stop = flag.Bool("stop", false, "stop the server from pid")
}
