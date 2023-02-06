// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"errors"
	"log"
	"net"
	"syscall"

	"github.com/luyu6056/gnet/internal/netpoll"
	"golang.org/x/sys/unix"
)

type ClientManage struct {
	*server
}

func (svr *ClientManage) Dial(network, addr string) (Conn, error) {

	if network == "tcp4" {
		tcpaddr, err := net.ResolveTCPAddr(network, addr)
		if err != nil {
			return nil, err
		}
		nfd, err := unixDialTcp(tcpaddr)
		if err != nil {
			return nil, err
		}
		lp := svr.server.subLoopGroup.next()
		c := newTCPConn(nfd, lp, nil)
		if svr.opts.TCPNoDelay {
			if err := unix.SetsockoptInt(nfd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
				return nil, err
			}
		}

		sa, _ := syscall.Getsockname(nfd)
		c.localAddr = &net.TCPAddr{IP: sa.(*syscall.SockaddrInet4).Addr[0:], Port: sa.(*syscall.SockaddrInet4).Port}
		c.remoteAddr = tcpaddr
		c.isClient = true
		_ = lp.svr.mainLoop.poller.Trigger(lp.addread, c)
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

	out, action := lp.eventHandler.OnOpened(c)
	if out != nil {
		c.write(out)
	}
	return lp.handleAction(c, action)
}
func client(eventHandler EventHandler, options *Options) *ClientManage {
	svr := &server{mainLoop: &eventloop{}}
	svr.mainLoop.svr = svr

	svr.subLoopGroup = new(eventLoopGroup)
	svr.eventHandler = eventHandler
	svr.connections = make([]*conn, 256)
	svr.opts = options
	svr.ln = &listener{fd: -1}
	svr.codec = func() ICodec {
		if options.Codec == nil {
			return new(BuiltInFrameCodec)
		}
		return options.Codec
	}()
	if p, err := netpoll.OpenPoller(); err == nil {
		el := &eventloop{
			idx:          0,
			svr:          svr,
			codec:        svr.codec,
			poller:       p,
			packet:       make([]byte, 0xFFFF),
			eventHandler: svr.eventHandler,
		}
		
		svr.mainLoop = el
		svr.subLoopGroup.register(el)
		svr.startLoops()
	} else {
		log.Fatalln("无法启动client")
	}
	return &ClientManage{svr}
}
