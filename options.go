// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package gnet

import (
	"math"
	"time"

	"github.com/luyu6056/tls"
)

// Option is a function that will set up option.
type Option func(opts *Options)

func initOptions(options ...Option) *Options {
	opts := new(Options)
	opts.WriteTimeOut = math.MaxInt32
	opts.PidName = "pid"
	for _, option := range options {
		option(opts)
	}
	return opts
}

// Options are set when the client opens.
type Options struct {

	//0 will be runtime.NumCPU().
	LoopNum int

	// Multicore indicates whether the engine will be effectively created with multi-cores, if so,
	// then you must take care with synchronizing memory between all event callbacks, otherwise,
	// it will run the engine with single thread. The number of threads in the engine will be
	// automatically assigned to the number of usable logical CPUs that can be leveraged by the
	// current process.
	Multicore bool

	// ReusePort indicates whether to set up the SO_REUSEPORT socket option.
	ReusePort bool

	// Ticker indicates whether the ticker has been set up.
	Ticker bool

	// TCPKeepAlive (SO_KEEPALIVE) socket option.
	TCPKeepAlive time.Duration

	// ICodec encodes and decodes TCP stream.
	Codec ICodec

	Isblock bool

	Tlsconfig *tls.Config

	TCPNoDelay bool

	WriteTimeOut int

	PidName string

	Graceful bool
}

// WithOptions sets up all options.
func WithOptions(options Options) Option {
	return func(opts *Options) {
		*opts = options
	}
}

// WithMulticore sets up multi-cores with gnet.
func WithLoopNum(n int) Option {
	return func(opts *Options) {
		opts.LoopNum = n
	}
}
func WithMulticore(multicore bool) Option {
	return func(opts *Options) {
		opts.Multicore = multicore
	}
}

// WithReusePort sets up SO_REUSEPORT socket option.
func WithReusePort(reusePort bool) Option {
	return func(opts *Options) {
		opts.ReusePort = reusePort
	}
}

// WithTCPKeepAlive sets up SO_KEEPALIVE socket option.
func WithTCPKeepAlive(tcpKeepAlive time.Duration) Option {
	return func(opts *Options) {
		opts.TCPKeepAlive = tcpKeepAlive
	}
}

// WithTicker indicates that a ticker is set.
func WithTicker(ticker bool) Option {
	return func(opts *Options) {
		opts.Ticker = ticker
	}
}

// WithCodec sets up a codec to handle TCP stream.
func WithCodec(codec ICodec) Option {
	return func(opts *Options) {
		opts.Codec = codec
	}
}

// 设置为阻塞式
func WithBlock(block bool) Option {
	return func(opts *Options) {
		opts.Isblock = block
	}
}

// 开启tls模式
func WithTls(tlsconfig *tls.Config) Option {
	return func(opts *Options) {
		opts.Tlsconfig = tlsconfig
	}
}

func WithTCPNoDelay(b bool) Option {
	return func(opts *Options) {
		opts.TCPNoDelay = b
	}
}

// 单位秒
func WithWriteTimeOut(i int) Option {
	return func(opts *Options) {
		opts.WriteTimeOut = i
	}
}

func WithPidName(name string) Option {
	return func(opts *Options) {
		opts.PidName = name
	}
}
func WithGraceful(graceful bool) Option {
	return func(opts *Options) {
		opts.Graceful = graceful
	}
}
