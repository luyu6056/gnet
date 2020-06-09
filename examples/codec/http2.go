package server

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/luyu6056/gnet"
	"github.com/luyu6056/gnet/tls"
	"github.com/panjf2000/ants/v2"
	"golang.org/x/net/http2/hpack"
)

type Http2server struct {
	c        gnet.Conn
	InHeader http2FrameHeader
	Setting  struct {
		HEADER_TABLE_SIZE            int   //允许发送者以八位字节的形式通知远程端点用于解码头块的头压缩表的最大尺寸。编码器可以通过使用特定于头部块内头部压缩格式的信令来选择等于或小于此值的任何大小
		ENABLE_PUSH                  bool  //此设置可用于禁用服务器推送
		SETTINGS_INITIAL_WINDOW_SIZE int32 //初始化默认的窗口大小
		MAX_FRAME_SIZE               int   //指示发送者愿意接收的最大帧有效载荷的大小
		MAX_HEADER_LIST_SIZE         int   //此通报设置以八位字节的形式通知对等方发送方准备接受的标题列表的最大大小
	}
	WorkStream      *Http2stream
	ReadPool        *ants.Pool
	SendPool        *ants.PoolWithFunc
	Streams         []*Http2stream
	ReadMetaHeaders *hpack.Decoder
	last_stream_id  uint32
	fps             uint32 //Frames Per Second,避免一些如CVE-2019-9512和CVE-2019-9515 ddos攻击，其实是限制客户端帧请求
	//IN_WINDOW_SIZE  int32  //接受到的窗口允许大小
	//OUT_WINDOW_SIZE int32  //发送出去的窗口允许大小
	lock sync.Mutex
}
type Http2stream struct {
	Out                             *tls.MsgBuffer
	Headers                         []hpack.HeaderField
	In                              *tls.MsgBuffer
	Id                              uint32
	IN_WINDOW_SIZE, OUT_WINDOW_SIZE int32
	sendch                          chan int8
	svr                             *Http2server
	close                           int8
	henc                            *hpack.Encoder
	headerbuf                       tls.MsgBuffer
	data                            *bytes.Reader
	compressbuf                     *tls.MsgBuffer
}

const (
	http2streamflagadd = iota
	http2streamflagclose
)

var http2initconn_setting = http2writeSettings{
	{http2SettingMaxFrameSize, http2defaultMaxReadFrameSize},
	{http2SettingMaxConcurrentStreams, http2MaxConcurrentStreams},
	{http2SettingMaxHeaderListSize, http2initialHeaderTableSize},
	//{http2SettingInitialWindowSize, http2initialWindowSize},
}
var http2initconn_windows_update = http2writeWindow_Update{size: http2serverWindowSize, streamId: 0}

//初始化一个连接
func NewH2Conn(c gnet.Conn) *Http2server {
	h2s := Http2pool.Get().(*Http2server)
	h2s.Setting.HEADER_TABLE_SIZE = http2initialHeaderTableSize
	h2s.Setting.ENABLE_PUSH = true
	h2s.Setting.MAX_FRAME_SIZE = http2initialMaxFrameSize
	h2s.Setting.SETTINGS_INITIAL_WINDOW_SIZE = http2initialWindowSize
	h2s.Setting.MAX_HEADER_LIST_SIZE = 0
	h2s.Streams[0].OUT_WINDOW_SIZE = http2initialWindowSize
	h2s.Streams[0].IN_WINDOW_SIZE = http2serverWindowSize
	h2s.Streams[0].Out.Reset()
	h2s.SendPool.Tune(http2MaxConcurrentStreams)
	h2s.SendPool.Reboot()
	h2s.c = c
	h2s.Streams[0].svr = h2s
	h2s.last_stream_id = 0
	http2initconn_setting.writeFrame(h2s.Streams[0])
	http2initconn_windows_update.writeFrame(h2s.Streams[0])
	return h2s
}
func (h2s *Http2server) Close() {
	h2s.SendPool.Release()

	for h2s.SendPool.Running() > 0 {
		for _, stream := range h2s.Streams {
			if stream != nil {
				select {
				case stream.sendch <- http2streamflagclose:
				default:
				}
			}
		}
		time.Sleep(time.Second)
	}

	Http2pool.Put(h2s)
}

const (
	http2serverWindowSize       = (1 << 31) - 1 - http2initialWindowSize //服务器窗口
	http2fpslimit               = 100                                    //帧率限制，避免ddos攻击
	http2WindowsSizeWaitTimeout = time.Second * 10                       //当窗口值为负的时候，会chan等待新的窗口增加，异步运行的时候也许会出现updatewindows没发送chan，导致流的chan锁死，所以为负值时候update会强行发送chan，但是也怕强行发送chan卡死，所以加了个超时
	http2MaxConcurrentStreams   = 256
	http2headerlength           = 9
	// ClientPreface is the string that must be sent by new
	// connections from clients.
	http2ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	// SETTINGS_MAX_FRAME_SIZE default
	// http://http2.github.io/http2-spec/#rfc.section.6.5.2
	http2initialMaxFrameSize = 16384

	// NextProtoTLS is the NPN/ALPN protocol negotiated during
	// HTTP/2's TLS setup.
	http2NextProtoTLS = "h2"

	// http://http2.github.io/http2-spec/#SettingValues
	http2initialHeaderTableSize = 4096

	http2initialWindowSize = 65536 // 6.9.2 Initial Flow Control Window Size

	http2defaultMaxReadFrameSize = 1 << 20
)

type http2FrameHeader struct {
	//valid    bool // caller can access []byte fields in the Frame
	Type     uint8
	Flags    uint8
	Length   int
	StreamID uint32
}
type http2FrameType uint8

const (
	// Data Frame
	http2FlagDataEndStream = 0x1
	http2FlagDataPadded    = 0x8

	// Headers Frame
	http2FlagHeadersEndStream  = 0x1
	http2FlagHeadersEndHeaders = 0x4
	http2FlagHeadersPadded     = 0x8
	http2FlagHeadersPriority   = 0x20

	// Settings Frame
	http2FlagSettingsAck = 0x1

	// Ping Frame
	http2FlagPingAck = 0x1

	// Continuation Frame
	http2FlagContinuationEndHeaders = 0x4

	http2FlagPushPromiseEndHeaders = 0x4
	http2FlagPushPromisePadded     = 0x8
)
const (
	http2FrameData         = 0x0
	http2FrameHeaders      = 0x1
	http2FramePriority     = 0x2
	http2FrameRSTStream    = 0x3
	http2FrameSettings     = 0x4
	http2FramePushPromise  = 0x5
	http2FramePing         = 0x6
	http2FrameGoAway       = 0x7
	http2FrameWindowUpdate = 0x8
	http2FrameContinuation = 0x9
)

type http2ErrCode uint32

const (
	http2ErrCodeNo                 http2ErrCode = 0x0
	http2ErrCodeProtocol           http2ErrCode = 0x1
	http2ErrCodeInternal           http2ErrCode = 0x2
	http2ErrCodeFlowControl        http2ErrCode = 0x3
	http2ErrCodeSettingsTimeout    http2ErrCode = 0x4
	http2ErrCodeStreamClosed       http2ErrCode = 0x5
	http2ErrCodeFrameSize          http2ErrCode = 0x6
	http2ErrCodeRefusedStream      http2ErrCode = 0x7
	http2ErrCodeCancel             http2ErrCode = 0x8
	http2ErrCodeCompression        http2ErrCode = 0x9
	http2ErrCodeConnect            http2ErrCode = 0xa
	http2ErrCodeEnhanceYourCalm    http2ErrCode = 0xb
	http2ErrCodeInadequateSecurity http2ErrCode = 0xc
	http2ErrCodeHTTP11Required     http2ErrCode = 0xd
)

var http2errCodeName = map[http2ErrCode]string{
	http2ErrCodeNo:                 "NO_ERROR",
	http2ErrCodeProtocol:           "PROTOCOL_ERROR",
	http2ErrCodeInternal:           "INTERNAL_ERROR",
	http2ErrCodeFlowControl:        "FLOW_CONTROL_ERROR",
	http2ErrCodeSettingsTimeout:    "SETTINGS_TIMEOUT",
	http2ErrCodeStreamClosed:       "STREAM_CLOSED",
	http2ErrCodeFrameSize:          "FRAME_SIZE_ERROR",
	http2ErrCodeRefusedStream:      "REFUSED_STREAM",
	http2ErrCodeCancel:             "CANCEL",
	http2ErrCodeCompression:        "COMPRESSION_ERROR",
	http2ErrCodeConnect:            "CONNECT_ERROR",
	http2ErrCodeEnhanceYourCalm:    "ENHANCE_YOUR_CALM",
	http2ErrCodeInadequateSecurity: "INADEQUATE_SECURITY",
	http2ErrCodeHTTP11Required:     "HTTP_1_1_REQUIRED",
}

const (
	http2SettingHeaderTableSize      = 0x1
	http2SettingEnablePush           = 0x2
	http2SettingMaxConcurrentStreams = 0x3
	http2SettingInitialWindowSize    = 0x4
	http2SettingMaxFrameSize         = 0x5
	http2SettingMaxHeaderListSize    = 0x6
)

type http2ConnectionError http2ErrCode

func (e http2ConnectionError) Error() string {
	return fmt.Sprintf("connection error: %s", http2ErrCode(e))
}

var Http2pool = sync.Pool{New: func() interface{} {
	hs := &Http2server{}
	hs.ReadPool, _ = ants.NewPool(http2MaxConcurrentStreams)
	hs.SendPool, _ = ants.NewPoolWithFunc(http2MaxConcurrentStreams, sendpool_static)
	hs.Streams = make([]*Http2stream, http2MaxConcurrentStreams)
	hs.Streams[0] = &Http2stream{sendch: make(chan int8), Out: &tls.MsgBuffer{}}
	hs.ReadMetaHeaders = hpack.NewDecoder(http2initialHeaderTableSize, nil)
	hs.Streams[0].svr = hs
	//hs.Request.Header = make(map[string]string)
	return hs
}}

//发送一个goaway顺便返回error
func (h2s *Http2server) connError(code http2ErrCode) error {
	stream := h2s.Streams[0]
	stream.Out.Reset()
	outbuf := stream.Out.Make(http2headerlength + 8)
	outbuf[0] = 0
	outbuf[1] = 0
	outbuf[2] = 4
	outbuf[3] = http2FrameGoAway
	outbuf[4] = 0
	outbuf[5] = 0
	outbuf[6] = 0
	outbuf[7] = 0
	outbuf[8] = 0
	outbuf[9] = byte(h2s.last_stream_id >> 24)
	outbuf[10] = byte(h2s.last_stream_id >> 16)
	outbuf[11] = byte(h2s.last_stream_id >> 8)
	outbuf[12] = byte(h2s.last_stream_id)
	outbuf[13] = byte(h2s.last_stream_id >> 24)
	outbuf[14] = byte(code >> 16)
	outbuf[15] = byte(code >> 8)
	outbuf[16] = byte(code)
	h2s.c.AsyncWrite(outbuf)
	return http2ConnectionError(code)
}

var stream_pool = sync.Pool{New: func() interface{} {
	hs := &Http2stream{Out: &tls.MsgBuffer{}, In: tls.NewBuffer(0)}
	hs.sendch = make(chan int8)
	hs.data = &bytes.Reader{}
	hs.compressbuf = &tls.MsgBuffer{}
	return hs
}}

type http2writeSettings [][2]int

func (settings http2writeSettings) writeFrame(stream *Http2stream) (err error) {
	msglen := len(settings) * 6
	outbuf := stream.Out.Make(msglen + http2headerlength)
	outbuf[0] = byte(msglen >> 16)
	outbuf[1] = byte(msglen >> 8)
	outbuf[2] = byte(msglen)
	outbuf[3] = http2FrameSettings
	outbuf[4] = 0
	outbuf[5] = 0
	outbuf[6] = 0
	outbuf[7] = 0
	outbuf[8] = 0
	var be = http2headerlength
	for _, setting := range settings {
		outbuf[be] = byte(setting[0] >> 8)
		outbuf[be+1] = byte(setting[0])
		outbuf[be+2] = byte(setting[1] >> 24)
		outbuf[be+3] = byte(setting[1] >> 16)
		outbuf[be+4] = byte(setting[1] >> 8)
		outbuf[be+5] = byte(setting[1])
		be += 6
	}

	return
}

type http2writeSettingsAck struct{}

func (settings http2writeSettingsAck) writeFrame(stream *Http2stream) (err error) {
	outbuf := stream.Out.Make(http2headerlength)
	outbuf[0] = 0
	outbuf[1] = 0
	outbuf[2] = 0
	outbuf[3] = http2FrameSettings
	outbuf[4] = http2FlagSettingsAck
	outbuf[5] = 0
	outbuf[6] = 0
	outbuf[7] = 0
	outbuf[8] = 0
	return
}

var (
	headerField_hsts                 = hpack.HeaderField{Name: "strict-transport-security", Value: "max-age= 31536000"}
	headerField_status200            = hpack.HeaderField{Name: ":status", Value: "200"}
	headerField_status206            = hpack.HeaderField{Name: ":status", Value: "206"}
	headerField_status404            = hpack.HeaderField{Name: ":status", Value: "404"}
	headerField_cachecontrol         = hpack.HeaderField{Name: "cache-control", Value: "max-age=86400"}
	headerField_nocache              = hpack.HeaderField{Name: "cache-control", Value: "no-store, no-cache, must-revalidate, max-age=0, s-maxage=0"}
	headerField_server               = hpack.HeaderField{Name: "server", Value: "gnet by luyu6056"}
	headerField_deflate              = hpack.HeaderField{Name: "content-encoding", Value: "deflate"}
	headerField_gzip                 = hpack.HeaderField{Name: "content-encoding", Value: "gzip"}
	headerField_status304            = hpack.HeaderField{Name: ":status", Value: "304"}
	headerField_firefox              = hpack.HeaderField{Name: "X-Firefox-Spdy", Value: "h2"}
	headerField_content_type_png     = hpack.HeaderField{Name: "content-type", Value: "image/png"}
	headerField_content_type_gif     = hpack.HeaderField{Name: "content-type", Value: "image/gif"}
	headerField_content_type_js      = hpack.HeaderField{Name: "content-type", Value: "application/javascript"}
	headerField_content_type_html    = hpack.HeaderField{Name: "content-type", Value: "text/html; charset=UTF-8"}
	headerField_content_type_css     = hpack.HeaderField{Name: "content-type", Value: "text/css"}
	headerField_content_type_default = hpack.HeaderField{Name: "content-type", Value: "application/octet-stream"}
	headerField_Accept_Ranges        = hpack.HeaderField{Name: "accept-ranges", Value: "bytes"}
)
var static_cache sync.Map

type file_cache struct {
	deflatefile     []byte
	content_type    string
	content_type_h2 hpack.HeaderField
	etag            string
	modTime         int64
	file            []byte
	check           uint32 //1秒钟检查1次
}

var sendpool_static = func(i interface{}) {
	stream, ok := i.(*Http2stream)
	if !ok || stream.Id == 0 { //主steam不允许处理data
		return
	}
	stream.headerbuf.Reset()
	stream.henc = hpack.NewEncoder(&stream.headerbuf)
	var filename string
	etag := ""
	var deflate, isgzip bool
	var range_start, range_end int
	for _, head := range stream.Headers {

		switch head.Name {
		case ":path":
			filename = head.Value
		case "accept-encoding":
			deflate = strings.Contains(head.Value, "deflate")
			if !deflate {
				isgzip = strings.Contains(head.Value, "gzip")
			}
		case "if-match", "if-none-match":
			etag = head.Value
		case "range":
			if strings.Index(head.Value, "bytes=") == 0 {

				if e := strings.Index(head.Value, "-"); e > 6 {
					range_start, _ = strconv.Atoi(head.Value[6:e])
					range_end, _ = strconv.Atoi(head.Value[e+1:])
				}

			}
		}
	}
	if index := strings.IndexByte(filename, '?'); index > 0 {
		filename = filename[:index]
	}
	if filename == "/" {
		filename = "/index.html"
	}
	switch filename {
	case "/hello":
		stream.henc.WriteField(headerField_status200)
		stream.henc.WriteField(headerField_nocache)
		stream.data.Reset([]byte("hello word!"))
		stream.WriteData(stream.data, stream.data.Len())
		return
	case "/getIP":
		stream.henc.WriteField(headerField_status200)
		stream.henc.WriteField(headerField_nocache)
		stream.data.Reset([]byte(`{"processedString":"` + stream.svr.c.RemoteAddr().String() + `"}`))
		stream.WriteData(stream.data, stream.data.Len())
		return
	case "/empty":
		stream.henc.WriteField(headerField_status200)
		stream.henc.WriteField(headerField_nocache)
		stream.data.Reset(nil)
		stream.WriteData(stream.data, stream.data.Len())
		return
	case "/garbage":

		stream.RandOut()
		return
	}
	filename = static_patch + filename
	var f_cache *file_cache
	if cache, ok := static_cache.Load(filename); ok {
		f_cache = cache.(*file_cache)
	} else {
		f_cache = &file_cache{}
	}
	var f *os.File
	var fstat os.FileInfo
	var err error
	//有缓存，检查文件是否修改
	if f_cache.etag != "" && atomic.CompareAndSwapUint32(&f_cache.check, 0, 1) {
		f, err = os.Open(filename)
		if err != nil {
			f_cache.etag = ""
			stream.Out404Frame(err)

			return
		}
		defer f.Close()
		fstat, err = f.Stat()
		if err != nil {
			f_cache.etag = ""
			stream.Out404Frame(err)

			return
		}
		if t := fstat.ModTime().Unix(); t > f_cache.modTime {
			f_cache.etag = ""
			f_cache.modTime = t
		}
		time.AfterFunc(time.Second, func() { f_cache.check = 0 })
	}
	if f_cache.etag != "" {
		if f_cache.etag == etag {
			stream.henc.WriteField(headerField_status304)
			stream.data.Reset(nil)
		} else if deflate && len(f_cache.deflatefile) > 0 {
			stream.henc.WriteField(headerField_status200)
			stream.henc.WriteField(f_cache.content_type_h2)
			stream.henc.WriteField(headerField_deflate)
			stream.data.Reset(f_cache.deflatefile)
		} else {
			stream.henc.WriteField(headerField_status200)
			stream.henc.WriteField(f_cache.content_type_h2)
			stream.data.Reset(f_cache.file)
		}
		stream.henc.WriteField(hpack.HeaderField{Name: "etag", Value: f_cache.etag})
		stream.WriteData(stream.data, stream.data.Len())

		return
	} else {
		//大文件时速度比较慢，目前的模式是小文件crc etag+缓存模式
		if f == nil {
			f, err = os.Open(filename)
			if err != nil {
				stream.Out404Frame(err)

				return
			}
			defer f.Close()
			fstat, err = f.Stat()
			if err != nil {
				stream.Out404Frame(err)

				return
			}
			f_cache.modTime = fstat.ModTime().Unix()
		}
		if fstat.Size() < 1024*1024*5 { //暂定5Mb是大文件
			b := make([]byte, fstat.Size())
			n, err := io.ReadFull(f, b)
			if err != nil || n != int(fstat.Size()) {
				stream.Out404Frame(err)

				return
			} else {
				f_cache.file = make([]byte, len(b))
				copy(f_cache.file, b)
				f_cache.etag = strconv.Itoa(int(crc32.ChecksumIEEE(b)))
				stream.henc.WriteField(headerField_status200)
				s := strings.Split(filename, ".")
				name := s[len(s)-1]
				switch {
				case strings.Contains(name, "css"):
					stream.henc.WriteField(headerField_content_type_css)
					f_cache.content_type_h2 = headerField_content_type_css
					f_cache.content_type = "text/css"
				case strings.Contains(name, "html"):
					stream.henc.WriteField(headerField_content_type_html)
					f_cache.content_type_h2 = headerField_content_type_html
					f_cache.content_type = "text/html;charset=utf-8"
				case strings.Contains(name, "js"):
					stream.henc.WriteField(headerField_content_type_js)
					f_cache.content_type_h2 = headerField_content_type_js
					f_cache.content_type = "application/javascript"
				case strings.Contains(name, "gif"):
					isgzip = false
					deflate = false
					stream.henc.WriteField(headerField_content_type_gif)
					f_cache.content_type_h2 = headerField_content_type_gif
					f_cache.content_type = "image/gif"
				case strings.Contains(name, "png"):
					isgzip = false
					deflate = false
					stream.henc.WriteField(headerField_content_type_png)
					f_cache.content_type_h2 = headerField_content_type_png
					f_cache.content_type = "image/png"
				default:
					isgzip = false
					deflate = false
					stream.henc.WriteField(headerField_content_type_default)
					f_cache.content_type_h2 = headerField_content_type_default
					f_cache.content_type = "application/octet-stream"
				}
				if len(b) > 512 && (isgzip || deflate) {
					switch {
					case deflate:
						stream.compressbuf.Reset()
						w := CompressNoContextTakeover(stream.compressbuf, 6)
						w.Write(b)
						w.Close()
						stream.henc.WriteField(headerField_deflate)
						f_cache.deflatefile = make([]byte, stream.compressbuf.Len())
						copy(f_cache.deflatefile, stream.compressbuf.Bytes())
						stream.data.Reset(stream.compressbuf.Bytes())
					case isgzip:
						g := gzippool.Get().(*gzip.Writer)
						stream.compressbuf.Reset()
						g.Reset(stream.compressbuf)
						g.Write(b)
						g.Flush()
						stream.henc.WriteField(headerField_gzip)
						stream.data.Reset(stream.compressbuf.Bytes())
						gzippool.Put(g)
					}
				} else {
					stream.data.Reset(b)
				}
				static_cache.Store(filename, f_cache)
				stream.henc.WriteField(hpack.HeaderField{Name: "etag", Value: f_cache.etag})
				stream.henc.WriteField(headerField_cachecontrol)
				stream.WriteData(stream.data, stream.data.Len())

			}
		} else {

			if range_start > 0 || range_end > 0 {

				stream.henc.WriteField(headerField_status206)
				if range_end == 0 {
					range_end = int(fstat.Size())
				}
				f.Seek(int64(range_start), 0)
				stream.henc.WriteField(headerField_content_type_default)
				stream.henc.WriteField(headerField_Accept_Ranges)
				stream.henc.WriteField(hpack.HeaderField{Name: "content-range", Value: "bytes " + strconv.Itoa(range_start) + "-" + strconv.Itoa(range_end) + "/" + strconv.Itoa(int(fstat.Size()))})
				stream.WriteData(f, range_end-range_start)
			} else {
				stream.henc.WriteField(headerField_status200)
				stream.henc.WriteField(headerField_content_type_default)
				stream.WriteData(f, int(fstat.Size()))
			}

		}

	}

}

func (stream *Http2stream) WriteData(reader io.Reader, length int) {

	stream.henc.WriteField(headerField_server)
	stream.henc.WriteField(headerField_hsts)

	stream.henc.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(length)})
	//stream.henc.WriteField(headerField_firefox)
	stream.writeFrame(reader, length)
	if stream.close == 3 {
		stream.svr.Streams[stream.Id] = nil
		stream_pool.Put(stream)
	}
}
func (stream *Http2stream) Out404Frame(err error) {
	stream.henc.WriteField(headerField_status404)
	stream.data.Reset(http404b)
	stream.henc.WriteField(headerField_server)
	stream.henc.WriteField(headerField_hsts)
	stream.henc.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(len(http404b))})
	stream.writeFrame(stream.data, len(http404b))
	if stream.close == 3 {
		stream.svr.Streams[stream.Id] = nil
		stream_pool.Put(stream)
	}
}
func (stream *Http2stream) writeFrame(reader io.Reader, msglen int) (err error) {

	makelen := stream.headerbuf.Len()
	stream.Out.Reset()
	buf := stream.Out.Make(http2headerlength + makelen)
	buf[0] = byte(makelen >> 16)
	buf[1] = byte(makelen >> 8)
	buf[2] = byte(makelen)
	buf[3] = http2FrameHeaders
	buf[4] = http2FlagHeadersEndHeaders
	buf[5] = byte(stream.Id >> 24)
	buf[6] = byte(stream.Id >> 16)
	buf[7] = byte(stream.Id >> 8)
	buf[8] = byte(stream.Id)
	copy(buf[http2headerlength:], stream.headerbuf.Bytes())
	if msglen == 0 || makelen >= stream.svr.Setting.MAX_FRAME_SIZE {

		buf[4] |= http2FlagHeadersEndStream
		stream.close |= 2
		stream.svr.c.AsyncWrite(buf)
		stream.Out.Reset()

	}
	for olen := stream.Out.Len(); msglen > 0; olen = stream.Out.Len() {

		makelen = stream.svr.Setting.MAX_FRAME_SIZE - olen
		outbuf := stream.Out.Make(http2headerlength + makelen)
		n, err := reader.Read(outbuf[http2headerlength:])

		if err != nil || n <= 0 {
			http2writeRST_STREAM(http2ErrCodeInternal).writeFrame(stream)
			return err
		}
		//总的流量窗口
		stream_windows_size := atomic.LoadInt32(&stream.svr.Streams[0].OUT_WINDOW_SIZE)
		if stream_windows_size <= 0 {

			select {
			case flag := <-stream.svr.Streams[0].sendch: //等待放行
				if flag == http2streamflagclose {
					return nil
				}

			}
			stream_windows_size = atomic.AddInt32(&stream.svr.Streams[0].OUT_WINDOW_SIZE, int32(-1*n))
			if stream_windows_size > 0 {
				select {
				case stream.svr.Streams[0].sendch <- http2streamflagadd:
				default:
				}
			}
		} else {
			atomic.AddInt32(&stream.svr.Streams[0].OUT_WINDOW_SIZE, int32(-1*n))
		}
		stream_windows_size = atomic.LoadInt32(&stream.OUT_WINDOW_SIZE)
		if stream_windows_size <= 0 {
			select {
			case flag := <-stream.sendch: //等待放行
				if flag == http2streamflagclose {
					return nil
				}

			}
		}

		msglen -= n
		atomic.AddInt32(&stream.OUT_WINDOW_SIZE, int32(-1*n))

		outbuf[0] = byte(n >> 16)
		outbuf[1] = byte(n >> 8)
		outbuf[2] = byte(n)
		outbuf[3] = http2FrameData
		outbuf[4] = 0
		outbuf[5] = byte(stream.Id >> 24)
		outbuf[6] = byte(stream.Id >> 16)
		outbuf[7] = byte(stream.Id >> 8)
		outbuf[8] = byte(stream.Id)

		if msglen == 0 {
			outbuf[4] = http2FlagDataEndStream
			stream.close |= 2
		}
		stream.svr.c.AsyncWrite(stream.Out.Next(olen + http2headerlength + n))
		stream.Out.Reset()
	}

	return nil
}

type http2writePing struct{}

func (http2writePing) writeFrame(stream *Http2stream) (err error) {
	outbuf := stream.Out.Make(http2headerlength + 8)
	outbuf[0] = 0
	outbuf[1] = 0
	outbuf[2] = 8
	outbuf[3] = http2FramePing
	outbuf[4] = http2FlagPingAck
	outbuf[5] = 0
	outbuf[6] = 0
	outbuf[7] = 0
	outbuf[8] = 0
	outbuf[9] = 0
	outbuf[10] = 0
	outbuf[11] = 0
	outbuf[12] = 0
	outbuf[13] = 0
	outbuf[14] = 0
	outbuf[15] = 0
	outbuf[16] = 0
	return nil
}

type http2writeWindow_Update struct {
	size     int32
	streamId uint32
}

func (svr http2writeWindow_Update) writeFrame(stream *Http2stream) (err error) {
	outbuf := stream.Out.Make(http2headerlength + 4)
	outbuf[0] = 0
	outbuf[1] = 0
	outbuf[2] = 4
	outbuf[3] = http2FrameWindowUpdate
	outbuf[4] = 0
	outbuf[5] = byte(svr.streamId >> 24)
	outbuf[6] = byte(svr.streamId >> 16)
	outbuf[7] = byte(svr.streamId >> 8)
	outbuf[8] = byte(svr.streamId)
	outbuf[9] = byte(svr.size >> 24)
	outbuf[10] = byte(svr.size >> 16)
	outbuf[11] = byte(svr.size >> 8)
	outbuf[12] = byte(svr.size)
	return nil
}

type http2writeRST_STREAM uint32

func (errcode http2writeRST_STREAM) writeFrame(stream *Http2stream) (err error) {
	stream.Out.Reset()
	outbuf := stream.Out.Make(http2headerlength + 4)
	outbuf[0] = 0
	outbuf[1] = 0
	outbuf[2] = 4
	outbuf[3] = http2FrameRSTStream
	outbuf[4] = 0
	outbuf[5] = byte(stream.Id >> 24)
	outbuf[6] = byte(stream.Id >> 16)
	outbuf[7] = byte(stream.Id >> 8)
	outbuf[8] = byte(stream.Id)
	outbuf[9] = byte(errcode >> 24)
	outbuf[10] = byte(errcode >> 16)
	outbuf[11] = byte(errcode >> 8)
	outbuf[12] = byte(errcode)
	stream.svr.c.AsyncWrite(outbuf)
	return nil
}
func (stream *Http2stream) RandOut() {
	f, err := os.Open(static_patch + "/tmp")
	if err != nil {
		stream.Out404Frame(err)
		return
	}
	defer f.Close()
	f_info, err := f.Stat()
	if err != nil {
		stream.Out404Frame(err)
		return
	}
	randlen := 1024*1024*20 + rand.Intn(1024*1024*40) //生成的随机长度，10+10MB
	stream.henc.WriteField(headerField_status200)
	stream.henc.WriteField(headerField_nocache)
	stream.henc.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(randlen)})
	//stream.henc.WriteField(headerField_firefox)
	makelen := stream.headerbuf.Len()
	stream.Out.Reset()
	buf := stream.Out.Make(http2headerlength + makelen)
	buf[0] = byte(makelen >> 16)
	buf[1] = byte(makelen >> 8)
	buf[2] = byte(makelen)
	buf[3] = http2FrameHeaders
	buf[4] = http2FlagHeadersEndHeaders
	buf[5] = byte(stream.Id >> 24)
	buf[6] = byte(stream.Id >> 16)
	buf[7] = byte(stream.Id >> 8)
	buf[8] = byte(stream.Id)
	copy(buf[http2headerlength:], stream.headerbuf.Bytes())

	for msglen, olen := randlen, stream.Out.Len(); randlen > 0; msglen, olen = randlen, stream.Out.Len() {
		if msglen > http2initialMaxFrameSize-olen-http2headerlength { //切分为一个tls包
			msglen = http2initialMaxFrameSize - olen - http2headerlength
		}
		outbuf := stream.Out.Make(http2headerlength + msglen)
		//设置随机起点
		f.Seek(rand.Int63n(f_info.Size()-int64(msglen)), 0)
		//读取一段长度
		n, err := f.Read(outbuf[http2headerlength:])

		if err != nil || n <= 0 {
			http2writeRST_STREAM(http2ErrCodeInternal).writeFrame(stream)
			return
		}
		//总的流量窗口
		stream_windows_size := atomic.LoadInt32(&stream.svr.Streams[0].OUT_WINDOW_SIZE)
		if stream_windows_size <= 0 {

			select {
			case flag := <-stream.svr.Streams[0].sendch: //等待放行
				if flag == http2streamflagclose {
					return
				}

			}
			stream_windows_size = atomic.AddInt32(&stream.svr.Streams[0].OUT_WINDOW_SIZE, int32(-1*n))
			if stream_windows_size > 0 {
				select {
				case stream.svr.Streams[0].sendch <- http2streamflagadd:
				default:
				}
			}
		} else {
			atomic.AddInt32(&stream.svr.Streams[0].OUT_WINDOW_SIZE, int32(-1*n))
		}
		stream_windows_size = atomic.LoadInt32(&stream.OUT_WINDOW_SIZE)
		if stream_windows_size <= 0 {
			select {
			case flag := <-stream.sendch: //等待放行
				if flag == http2streamflagclose {
					return
				}

			}
		}
		randlen -= n
		atomic.AddInt32(&stream.OUT_WINDOW_SIZE, int32(-1*n))

		outbuf[0] = byte(n >> 16)
		outbuf[1] = byte(n >> 8)
		outbuf[2] = byte(n)
		outbuf[3] = http2FrameData
		outbuf[4] = 0
		outbuf[5] = byte(stream.Id >> 24)
		outbuf[6] = byte(stream.Id >> 16)
		outbuf[7] = byte(stream.Id >> 8)
		outbuf[8] = byte(stream.Id)

		if randlen == 0 {
			outbuf[4] = http2FlagDataEndStream
			stream.close |= 2
		}
		stream.svr.c.AsyncWrite(stream.Out.Next(olen + http2headerlength + n))
		stream.Out.Reset()

	}

	stream.svr.Streams[stream.Id] = nil
	stream_pool.Put(stream)
}
