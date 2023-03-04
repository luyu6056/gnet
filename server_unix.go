// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
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
	connections      sync.Map // loop connections fd -> conn
	connWg           *sync.WaitGroup
}

// waitForShutdown waits for a signal to shutdown
func (srv *server) waitForShutdown() {
	srv.cond.L.Lock()
	srv.cond.Wait()
	srv.cond.L.Unlock()
	srv.stop()
}

// signalShutdown signals a shutdown an begins server closing
func (srv *server) signalShutdown() {
	srv.once.Do(func() {
		srv.cond.L.Lock()
		srv.cond.Signal()
		srv.cond.L.Unlock()
	})
}

func (srv *server) startLoops() {
	srv.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
		srv.wg.Add(1)
		go func() {
			lp.loopOut()
			lp.loopRun()
			srv.wg.Done()
		}()
		return true
	})
}
func (srv *server) closeConns() {
	srv.connections.Range(func(key, value interface{}) bool {
		c := value.(*conn)
		c.loopCloseConn(errors.ErrEngineShutdown)
		return true
	})
	srv.connWg.Wait()

}
func (srv *server) closeLoops() {
	select {
	case srv.close <- true:
	default:

	}

	srv.closeConns()
	var wg sync.WaitGroup
	srv.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
		wg.Add(1)
		sniffError(lp.poller.Trigger(func(_ interface{}) error {

			return errors.ErrEngineShutdown
		}, nil))
		lp.outclose <- true
		go func() {
			<-lp.outclose
			wg.Done()
		}()

		return true
	})
	wg.Wait()
}

func (srv *server) startReactors() {
	srv.subLoopGroup.iterate(func(i int, el *eventloop) bool {
		srv.wg.Add(1)
		go func() {
			el.loopOut()
			srv.activateSubReactor(el)
			srv.wg.Done()
		}()
		return true
	})
}

func (srv *server) activateLoops(numLoops int) error {
	// Create loops locally and bind the listeners.

	for i := 0; i < numLoops; i++ {
		if p, err := netpoll.OpenPoller(); err == nil {
			el := &eventloop{
				idx:          i,
				srv:          srv,
				codec:        srv.codec,
				poller:       p,
				packet:       make([]byte, 0xFFFF),
				eventHandler: srv.eventHandler,
			}

			el.pollAttachment = netpoll.GetPollAttachment()
			el.pollAttachment.FD = srv.ln.fd
			el.pollAttachment.Callback = el.handleEvent
			_ = el.poller.AddRead(el.pollAttachment)
			srv.subLoopGroup.register(el)
		} else {
			return err
		}
	}

	srv.subLoopGroupSize = srv.subLoopGroup.len()
	// Start loops in background
	srv.startLoops()
	return nil
}

func (srv *server) activateReactors(numLoops int) error {
	if p, err := netpoll.OpenPoller(); err == nil {
		el := &eventloop{
			idx:      -1,
			poller:   p,
			srv:      srv,
			outclose: make(chan bool, 1),
		}
		el.pollAttachment = netpoll.GetPollAttachment()
		el.pollAttachment.FD = srv.ln.fd
		el.pollAttachment.Callback = srv.activateMainReactorCallback
		_ = el.poller.AddRead(el.pollAttachment)
		srv.mainLoop = el
		// Start main reactor.
		srv.wg.Add(1)
		go func() {

			srv.activateMainReactor()
			srv.wg.Done()
		}()
	} else {
		return err
	}
	for i := 0; i < numLoops; i++ {
		if p, err := netpoll.OpenPoller(); err == nil {
			el := &eventloop{
				idx:          i,
				srv:          srv,
				codec:        srv.codec,
				poller:       p,
				packet:       make([]byte, 0xFFFF),
				eventHandler: srv.eventHandler,
			}

			srv.subLoopGroup.register(el)
		} else {
			return err
		}
	}
	srv.subLoopGroupSize = srv.subLoopGroup.len()
	// Start sub reactors.
	srv.startReactors()

	return nil
}

func (srv *server) activateMainReactorCallback(fd int) error {
	return srv.acceptNewConnection(fd)
}

func (srv *server) start(numCPU int) error {
	if srv.opts.ReusePort || srv.ln.pconn != nil {
		return srv.activateLoops(numCPU)
	}
	return srv.activateReactors(numCPU)
}

func (srv *server) stop() {
	srv.waitClose()
	// Close loops and all outstanding connections
	srv.closeLoops()

	// Wait on all loops to complete reading events

	// Notify all loops to close by closing all listeners

	if srv.mainLoop != nil {
		sniffError(srv.mainLoop.poller.Trigger(func(_ interface{}) error {

			return errors.ErrEngineShutdown
		}, nil))
	}
	srv.wg.Wait()

	if srv.mainLoop != nil {
		sniffError(srv.mainLoop.poller.Close())
		srv.mainLoop.outclose <- true

	}
}

// tcp平滑重启，开启ReusePort有效，关闭ReusePort则会造成短暂的错误

func serve(eventHandler EventHandler, addr string, options *Options) error {
	srv := new(server)
	srv.connWg = new(sync.WaitGroup)
	var ln listener
	//efer ln.close()

	ln.network, ln.addr = parseAddr(addr)
	if len(ln.network) >= 4 && ln.network[:4] == "unix" {
		sniffError(os.RemoveAll(ln.addr))
	}
	var (
		reload, graceful, stop bool
	)
	if options.Graceful {
		flag.BoolVar(&reload, "reload", false, "listen on fd open 3 (internal use only)")
		flag.BoolVar(&graceful, "graceful", false, "listen on fd open 3 (internal use only)")
		flag.BoolVar(&stop, "stop", false, "stop the server from pid")
	}

	var err error
	if ln.network == "udp" {
		ln.pconn, err = net.ListenPacket(ln.network, ln.addr)
	} else {
		flag.Parse()
		if stop {
			b, err := ioutil.ReadFile("./" + options.PidName)
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
		if reload {
			b, err := ioutil.ReadFile("./" + options.PidName)
			if err == nil {
				pidstr := string(b)
				pid, err := strconv.Atoi(pidstr)
				if err == nil {
					if err = syscall.Kill(pid, syscall.SIGUSR1); err == nil {
						log.Println("reload ok")
						return nil
					} else {
						log.Println("server not start")
						return nil
					}
				} else {
					log.Println("server not start")
					return nil
				}
			}
		}
		if graceful {
			f := os.NewFile(3, "")
			ln.ln, err = net.FileListener(f)
			f.Close()
		} else {
			ln.ln, err = net.Listen(ln.network, ln.addr)
		}

		if err == nil {
			pid := unix.Getpid()
			f, err := os.OpenFile("./"+options.PidName, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
			if err != nil {
				return err
			}
			f.WriteString(strconv.Itoa(int(pid)))
			f.Close()
			if options.Graceful {
				go srv.signalHandler()
			}
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
	srv.close = make(chan bool, 1)

	srv.opts = options
	srv.tlsconfig = options.Tlsconfig
	srv.eventHandler = eventHandler
	srv.ln = &ln
	srv.subLoopGroup = new(eventLoopGroup)
	srv.cond = sync.NewCond(&sync.Mutex{})
	srv.ticktock = make(chan time.Duration, 1)
	srv.Isblock = options.Isblock
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
			srv.close <- true
		},
	}
	if srv.opts.ReusePort {
		err := unix.SetsockoptInt(srv.ln.fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err != nil {
			return err
		}

		err = unix.SetsockoptInt(srv.ln.fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		if err != nil {
			return err
		}
	}
	switch srv.eventHandler.OnInitComplete(server) {
	case None:
	case Shutdown:
		return nil
	}

	if err := srv.start(numCPU); err != nil {
		srv.closeLoops()
		log.Printf("gnet server is stoping with error: %v\n", err)
		return err
	}

	srv.waitForShutdown()

	return nil
}
func (srv *server) signalHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	select {
	case sig := <-ch:
		signal.Stop(ch)
		var wg, wg1 sync.WaitGroup
		wg.Add(srv.subLoopGroup.len())
		wg1.Add(1)
		srv.subLoopGroup.iterate(func(i int, lp *eventloop) bool {
			sniffError(lp.poller.Trigger(func(_ interface{}) error {
				wg.Done()
				wg1.Wait()
				return nil
			}, nil))
			return true
		})
		wg.Wait()
		srv.ln.fd = 0 // 修改监听fd让accept失效
		wg1.Done()
		// timeout context for shutdown
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			// stop
			log.Println("signal: stop")
			srv.signalShutdown()
			return
		case syscall.SIGUSR1:
			if srv.ln != nil {
				// reload
				f, err := srv.ln.ln.(*net.TCPListener).File()
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
				srv.signalShutdown()
			}

			return
		}
	case <-srv.close:
		log.Println("close gnet")
		srv.signalShutdown()
		return
	}

}

func (srv *server) waitClose() {

	var wg sync.WaitGroup
	srv.connections.Range(func(key, value interface{}) bool {
		c := value.(*conn)
		wg.Add(1)
		_ = c.loop.poller.Trigger(func(i interface{}) error {
			if c != nil {
				if c.state == connStateOk {
					srv.eventHandler.SignalClose(c)
				}
			}

			wg.Done()
			return nil
		}, nil)
		return true
	})
	wg.Wait()

}
