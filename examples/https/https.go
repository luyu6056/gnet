package main

import (
	"fmt"
	"log"
	"math/rand"
	"runtime"
	"time"

	"github.com/luyu6056/gnet"
	"github.com/luyu6056/gnet/examples/codec"
	"github.com/luyu6056/gnet/tls"
	"github.com/panjf2000/ants/v2"
)

type mainServer struct {
	*gnet.EventServer
	addr string
	pool *ants.Pool
}

var gopool, _ = ants.NewPool(1024, ants.WithPreAlloc(true))

func main() {
	rand.Seed(time.Now().Unix())
	cert, err := tls.LoadX509KeyPair("./server.crt", "./server.key")
	if err != nil {
		log.Fatalf("tls.LoadX509KeyPair err: %v", err)
	}
	http := &mainServer{addr: "tcp://:8080", pool: gopool}
	go gnet.Serve(http, http.addr, gnet.WithLoopNum(runtime.NumCPU()), gnet.WithReusePort(false), gnet.WithTCPKeepAlive(time.Second*600), gnet.WithCodec(&codec.Tlscodec{}), gnet.WithOutbuf(1024), gnet.WithMultiOut(true))
	h443 := &mainServer{addr: "tcp://:443", pool: gopool}
	log.Fatal(gnet.Serve(h443, h443.addr, gnet.WithLoopNum(runtime.NumCPU()), gnet.WithReusePort(false), gnet.WithTCPKeepAlive(time.Second*600), gnet.WithCodec(&codec.Tlscodec{}), gnet.WithOutbuf(1024), gnet.WithMultiOut(true), gnet.WithTls(&tls.Config{
		Certificates:             []tls.Certificate{cert},
		NextProtos:               []string{"h2", "http/1.1"},
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		MinVersion: tls.VersionTLS12,
	})))
}

func (hs *mainServer) OnInitComplete(srv gnet.Server) (action gnet.Action) {
	fmt.Printf("server started on %s (loops: %d)\r\n", hs.addr, srv.NumEventLoop)
	return
}

func (hs *mainServer) OnOpened(c gnet.Conn) (out []byte, action gnet.Action) {
	time.AfterFunc(time.Second*10, func() {
		if c.Context() == nil {
			c.Close()
		}
	})
	return
}

func (hs *mainServer) OnClosed(c gnet.Conn, err error) (action gnet.Action) {
	switch svr := c.Context().(type) {

	case *codec.Httpserver:
		codec.Httppool.Put(svr)
	case *codec.Http2server:
		if err == gnet.ErrServerShutdown {
			svr.Close()
		} else {
			svr.ReadPool.Submit(func() {
				svr.Close()
			})
		}
	}
	c.SetContext(nil)
	return
}

func (hs *mainServer) React(data []byte, c gnet.Conn) (action gnet.Action) {
	switch svr := c.Context().(type) {
	case *codec.Httpserver:
		switch svr.Request.Path {
		case "/hello":
			svr.Output_data([]byte("hello word!"))
		case "/getIP":
			svr.Output_data([]byte(`{"processedString":"` + c.RemoteAddr().String() + `"}`))
		case "/empty":
			svr.Output_data(nil)
		case "/garbage":
			svr.RandOut()
		case "/ws":
			err := svr.Upgradews(c)
			if err != nil {
				action = gnet.Close
				return
			}
			codec.Httppool.Put(svr)
			return gnet.None
		default:
			svr.Static()
		}
		if svr.Request.Connection == "close" {
			action = gnet.Close
		}
	case *codec.WSconn:
		svr.WriteMessage(codec.TextMessage, []byte("hello word!"))
	case *codec.Http2server:
		svr.SendPool.Invoke(svr.WorkStream) //h2是异步，可能会Jitter抖动厉害
	}
	return
}
