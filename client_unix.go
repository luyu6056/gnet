//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"errors"
	"log"
	"net"
	"sync"
	"syscall"

	"github.com/luyu6056/gnet/internal/netpoll"
	"golang.org/x/sys/unix"
)

type ClientManage struct {
	*server
}

func (srv *ClientManage) Dial(network, addr string) (Conn, error) {

	if network == "tcp4" {
		tcpaddr, err := net.ResolveTCPAddr(network, addr)
		if err != nil {
			return nil, err
		}
		nfd, err := unixDialTcp(tcpaddr)
		if err != nil {
			return nil, err
		}
		lp := srv.server.subLoopGroup.next()
		c := newTCPConn(nfd, lp, nil)
		if srv.opts.TCPNoDelay {
			if err := unix.SetsockoptInt(nfd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
				return nil, err
			}
		}

		sa, _ := syscall.Getsockname(nfd)
		c.localAddr = &net.TCPAddr{IP: sa.(*syscall.SockaddrInet4).Addr[0:], Port: sa.(*syscall.SockaddrInet4).Port}
		c.remoteAddr = tcpaddr
		c.isClient = true
		_ = lp.srv.mainLoop.poller.Trigger(lp.addread, c)
		return c, nil
	} else {

	}
	return nil, nil
}
func unixDialTcp(tcpAddr *net.TCPAddr) (int, error) {
	sockfd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, errors.New("创建socket失败 " + err.Error())
	}

	rsa := &syscall.SockaddrInet4{Addr: [4]byte{tcpAddr.IP[12], tcpAddr.IP[13], tcpAddr.IP[14], tcpAddr.IP[15]}, Port: tcpAddr.Port}
	err = syscall.Connect(sockfd, rsa)
	return sockfd, err
}

func (lp *eventloop) loopOpenClient(c *conn) error {
	c.state = connStateOk
	lp.srv.connWg.Add(1)
	out, action := lp.eventHandler.OnOpened(c)
	if out != nil {
		c.write(out)
	}
	return lp.handleAction(c, action)
}
func client(eventHandler EventHandler, options *Options) *ClientManage {
	srv := &server{mainLoop: &eventloop{}}
	srv.mainLoop.srv = srv
	srv.connWg = new(sync.WaitGroup)
	srv.subLoopGroup = new(eventLoopGroup)
	srv.eventHandler = eventHandler

	srv.opts = options
	srv.ln = &listener{fd: -1}
	srv.codec = func() ICodec {
		if options.Codec == nil {
			return new(BuiltInFrameCodec)
		}
		return options.Codec
	}()
	if p, err := netpoll.OpenPoller(); err == nil {
		el := &eventloop{
			idx:          0,
			srv:          srv,
			codec:        srv.codec,
			poller:       p,
			packet:       make([]byte, 0xFFFF),
			eventHandler: srv.eventHandler,
		}

		srv.mainLoop = el
		srv.subLoopGroup.register(el)
		srv.startLoops()
	} else {
		log.Fatalln("无法启动client")
	}
	return &ClientManage{srv}
}
