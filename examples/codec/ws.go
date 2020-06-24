package codec

import (
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/luyu6056/gnet/tls"
)

// The message types are defined in RFC 6455, section 11.8.
const (
	wsfpslimit = 200 //帧率限制
	// TextMessage denotes a text data message. The text message payload is
	// interpreted as UTF-8 encoded text data.
	TextMessage = 1

	// BinaryMessage denotes a binary data message.
	BinaryMessage = 2

	// CloseMessage denotes a close control message. The optional message
	// payload contains a numeric code and text. Use the FormatCloseMessage
	// function to format a close message payload.
	CloseMessage = 8

	// PingMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	PingMessage = 9

	// PongMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	PongMessage = 10

	// Frame header byte 0 bits from Section 5.2 of RFC 6455
	finalBit = 1 << 7
	rsv1Bit  = 1 << 6
	rsv2Bit  = 1 << 5
	rsv3Bit  = 1 << 4

	// Frame header byte 1 bits from Section 5.2 of RFC 6455
	maskBit                    = 1 << 7
	msgheader                  = 2
	msglength8                 = 8
	msglength2                 = 2
	msgmask                    = 4
	maxControlFramePayloadSize = 125

	defaultReadBufferSize  = 4096
	defaultWriteBufferSize = 8192

	continuationFrame         = 0
	continuationFramefinalBit = continuationFrame | finalBit
	BinaryMessagefinalBit     = BinaryMessage | finalBit
	noFrame                   = -1
)

// Close codes defined in RFC 6455, section 11.7.
const (
	CloseNormalClosure           = 1000
	CloseGoingAway               = 1001
	CloseProtocolError           = 1002
	CloseUnsupportedData         = 1003
	CloseNoStatusReceived        = 1005
	CloseAbnormalClosure         = 1006
	CloseInvalidFramePayloadData = 1007
	ClosePolicyViolation         = 1008
	CloseMessageTooBig           = 1009
	CloseMandatoryExtension      = 1010
	CloseInternalServerErr       = 1011
	CloseServiceRestart          = 1012
	CloseTryAgainLater           = 1013
	CloseTLSHandshake            = 1015
)

var (
	errWriteTimeout        = &netError{msg: "websocket: write timeout", timeout: true, temporary: true}
	errUnexpectedEOF       = &CloseError{Code: CloseAbnormalClosure, Text: io.ErrUnexpectedEOF.Error()}
	errBadWriteOpCode      = errors.New("websocket: bad write message type")
	errWriteClosed         = errors.New("websocket: write closed")
	errInvalidControlFrame = errors.New("websocket: invalid control frame")
	errrequest             = errors.New("request not ready")
)
var ErrReadLimit = errors.New("websocket: read limit exceeded")
var ErrCloseSent = errors.New("websocket: close sent")

type WSconn struct {
	Http *Httpserver
	//以下是发送接收相关
	IsCompress     bool
	readRemaining  int  // bytes remaining in current frame.
	ReadFinal      bool // true the current message has more frames.
	ReadLength     int  // Message size.
	readDecompress bool
	IsServer       bool
	Write          func([]byte) error
	readbuf        *tls.MsgBuffer //读取分帧和解压用
	messageType    int
	fps            uint32
	Ctx            interface{}
}

var buf_pool = sync.Pool{New: func() interface{} {
	return new(tls.MsgBuffer)
}}

func (c *WSconn) ReadMessage(in []byte) (frameType int, result []byte, err error) {
	// Close previous reader, only relevant for decompression.
	c.ReadLength = 0
	for {
		frameType, err = c.advanceFrame(in)
		if err != nil {
			err = hideTempErr(err)
			break
		}
		if frameType == noFrame {
			break
		}
		if frameType == TextMessage || frameType == BinaryMessage {
			c.messageType = frameType
		}
		if c.ReadFinal {
			if fps := atomic.AddUint32(&c.fps, 1); fps == 1 {
				time.AfterFunc(time.Second, func() { c.fps = 0 })
			} else if fps > wsfpslimit {
				return noFrame, nil, io.EOF
			}
			if c.readDecompress {
				c.readDecompress = false
				p, err := ioutil.ReadAll(DecompressNoContextTakeover(c.readbuf))
				return c.messageType, p, err
			}
			c.readDecompress = false

			return c.messageType, c.readbuf.Next(c.readbuf.Len()), nil
		}
	}
	return noFrame, nil, err
}
func (c *WSconn) advanceFrame(in []byte) (int, error) {

	// 1. Skip remainder of previous frame.

	/*if c.readRemaining > 0 {
		if _, err := io.CopyN(ioutil.Discard, c.br, int64(c.readRemaining)); err != nil {
			return noFrame, err
		}
	}*/

	// 2. Read and parse first two bytes of frame header.
	p := in[c.ReadLength:]
	var readlength int
	if len(p) < 2 {
		return noFrame, nil
	}
	p0 := p[0]
	final := p0&finalBit != 0
	frameType := int(p0 & 0xf)
	mask := p[1]&maskBit != 0
	c.readRemaining = int(p[1] & 0x7f)

	if c.IsCompress && (p0&rsv1Bit) != 0 {
		c.readDecompress = true
		p0 &^= rsv1Bit
	}
	if rsv := p0 & (rsv1Bit | rsv2Bit | rsv3Bit); rsv != 0 {
		return noFrame, c.handleProtocolError("unexpected reserved bits 0x" + strconv.FormatInt(int64(rsv), 16))
	}

	switch frameType {
	case CloseMessage, PingMessage, PongMessage:
		if c.readRemaining > maxControlFramePayloadSize {
			return noFrame, c.handleProtocolError("control frame length > 125")
		}
		if !final {
			return noFrame, c.handleProtocolError("control frame not final")
		}
	case TextMessage, BinaryMessage:
		if !c.ReadFinal {
			return noFrame, c.handleProtocolError("message start before final message frame")
		}
		c.ReadFinal = final
	case continuationFrame:
		if c.ReadFinal {
			return noFrame, c.handleProtocolError("continuation after final message frame")
		}
		c.ReadFinal = final
	default:
		return noFrame, c.handleProtocolError("unknown opcode " + strconv.Itoa(frameType))
	}

	// 3. Read and parse frame length.

	switch c.readRemaining {
	case 126:
		if len(p) < 4 {
			return noFrame, nil
		}
		c.readRemaining = int(p[2])<<8 | int(p[3])
		readlength = 4
	case 127:
		if len(p) < 10 {
			return noFrame, nil
		}
		c.readRemaining = int(p[2])<<56 | int(p[3])<<48 | int(p[4])<<40 | int(p[5])<<32 | int(p[6])<<24 | int(p[7])<<16 | int(p[8])<<8 | int(p[9])
		readlength = 8
	default:
		readlength = 2
	}

	// 4. Handle frame masking.

	if mask != c.IsServer {
		return noFrame, c.handleProtocolError("incorrect mask flag")
	}

	var payload []byte
	if mask {
		if len(p) < readlength+4+c.readRemaining {
			return noFrame, nil
		}
		maskkey := p[readlength : readlength+4]
		readlength += 4
		payload = p[readlength : readlength+c.readRemaining]
		for i, v := range payload {
			payload[i] = v ^ maskkey[i&3]
		}
	} else {
		if len(p) < c.readRemaining+readlength {
			return noFrame, nil
		}
		payload = p[readlength : c.readRemaining+readlength]
	}
	readlength += c.readRemaining
	c.ReadLength += readlength
	// 5. For text and binary messages, enforce read limit and return.
	if frameType == continuationFrame || frameType == TextMessage || frameType == BinaryMessage {
		//if c.readLimit > 0 && c.ReadLength > c.readLimit {
		//	c.WriteControl(CloseMessage, FormatCloseMessage(CloseMessageTooBig, ""), time.Now().Add(writeWait))
		//	return noFrame, ErrReadLimit
		//}

		c.readbuf.Write(payload)
		return frameType, nil
	}

	// 7. Process control frame payload.

	switch frameType {
	case PongMessage:
		//if err := c.handlePong(string(payload)); err != nil {
		//	return noFrame, err
		//}
	case PingMessage:
		err := c.WriteMessage(PongMessage, payload)
		if err == ErrCloseSent {
			err = nil
		} else if e, ok := err.(net.Error); ok && e.Temporary() {
			err = nil
		}
		if err != nil {
			return noFrame, err
		}
	case CloseMessage:

		closeCode := CloseNoStatusReceived
		closeText := ""
		if len(payload) >= 2 {
			closeCode = int(binary.BigEndian.Uint16(payload[:2]))

			if !isValidReceivedCloseCode(closeCode) {

				return noFrame, c.handleProtocolError("invalid close code")
			}
			closeText = string(payload[2:])
			if !utf8.ValidString(closeText) {

				return noFrame, c.handleProtocolError("invalid utf8 payload in close frame")
			}
		}

		message := []byte{}
		if closeCode != CloseNoStatusReceived {
			message = FormatCloseMessage(closeCode, "")
		}
		c.WriteMessage(CloseMessage, message)
		return noFrame, &CloseError{Code: closeCode, Text: closeText}
	}

	return frameType, nil
}
func hideTempErr(err error) error {
	if e, ok := err.(net.Error); ok && e.Temporary() {
		err = &netError{msg: e.Error(), timeout: e.Timeout()}
	}
	return err
}
func (c *WSconn) handleProtocolError(message string) error {
	c.WriteMessage(CloseMessage, FormatCloseMessage(CloseProtocolError, message))
	return errors.New("websocket: " + message)
}

// netError satisfies the net Error interface.
type netError struct {
	msg       string
	temporary bool
	timeout   bool
}

func (e *netError) Error() string   { return e.msg }
func (e *netError) Temporary() bool { return e.temporary }
func (e *netError) Timeout() bool   { return e.timeout }

// CloseError represents close frame.
type CloseError struct {

	// Code is defined in RFC 6455, section 11.7.
	Code int

	// Text is the optional text payload.
	Text string
}

func (e *CloseError) Error() string {
	s := []byte("websocket: close ")
	s = strconv.AppendInt(s, int64(e.Code), 10)
	switch e.Code {
	case CloseNormalClosure:
		s = append(s, " (normal)"...)
	case CloseGoingAway:
		s = append(s, " (going away)"...)
	case CloseProtocolError:
		s = append(s, " (protocol error)"...)
	case CloseUnsupportedData:
		s = append(s, " (unsupported data)"...)
	case CloseNoStatusReceived:
		s = append(s, " (no status)"...)
	case CloseAbnormalClosure:
		s = append(s, " (abnormal closure)"...)
	case CloseInvalidFramePayloadData:
		s = append(s, " (invalid payload data)"...)
	case ClosePolicyViolation:
		s = append(s, " (policy violation)"...)
	case CloseMessageTooBig:
		s = append(s, " (message too big)"...)
	case CloseMandatoryExtension:
		s = append(s, " (mandatory extension missing)"...)
	case CloseInternalServerErr:
		s = append(s, " (internal server error)"...)
	case CloseTLSHandshake:
		s = append(s, " (TLS handshake error)"...)
	}
	if e.Text != "" {
		s = append(s, ": "...)
		s = append(s, e.Text...)
	}
	return string(s)
}

// FormatCloseMessage formats closeCode and text as a WebSocket close message.
func FormatCloseMessage(closeCode int, text string) []byte {
	buf := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(buf, uint16(closeCode))
	copy(buf[2:], text)
	return buf
}

var validReceivedCloseCodes = map[int]bool{
	// see http://www.iana.org/assignments/websocket/websocket.xhtml#close-code-number

	CloseNormalClosure:           true,
	CloseGoingAway:               true,
	CloseProtocolError:           true,
	CloseUnsupportedData:         true,
	CloseNoStatusReceived:        false,
	CloseAbnormalClosure:         false,
	CloseInvalidFramePayloadData: true,
	ClosePolicyViolation:         true,
	CloseMessageTooBig:           true,
	CloseMandatoryExtension:      true,
	CloseInternalServerErr:       true,
	CloseServiceRestart:          true,
	CloseTryAgainLater:           true,
	CloseTLSHandshake:            false,
}

func isValidReceivedCloseCode(code int) bool {
	return validReceivedCloseCodes[code] || (code >= 3000 && code <= 4999)
}
func isControl(frameType int) bool {
	return frameType == CloseMessage || frameType == PingMessage || frameType == PongMessage
}
func newMaskKey() [4]byte {
	n := rand.Uint32()
	return [4]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
}

func (c *WSconn) Output_data(msg *tls.MsgBuffer) { //专用的server输出

	mw := write_pool.Get().(*messageWriter)
	mw.compress = false
	if c.IsCompress && msg.Len() > 256 { //太小的不压缩
		mw.compress = true
		mw.writeBuf.Reset()
		w := CompressNoContextTakeover(mw.writeBuf, 3)
		w.Write(msg.Bytes())
		w.Close()
		msg = mw.writeBuf //把压缩过的消息，写回msg
	}
	mw.outbuf[0] = BinaryMessagefinalBit
	if mw.compress {
		mw.outbuf[0] |= rsv1Bit
	}
	for msglen := msg.Len(); msglen > 0; msglen, mw.outbuf[0] = msg.Len(), continuationFramefinalBit {

		if msglen > defaultWriteBufferSize {
			msglen = defaultWriteBufferSize //当前帧长度，不大于帧大小
			mw.outbuf[0] -= mw.outbuf[0] & finalBit
		}

		switch {
		case msglen >= 65536:
			mw.outbuf[1] = 127
			mw.outbuf[2] = byte(msglen >> 56)
			mw.outbuf[3] = byte(msglen >> 48)
			mw.outbuf[4] = byte(msglen >> 40)
			mw.outbuf[5] = byte(msglen >> 32)
			mw.outbuf[6] = byte(msglen >> 24)
			mw.outbuf[7] = byte(msglen >> 16)
			mw.outbuf[8] = byte(msglen >> 8)
			mw.outbuf[9] = byte(msglen)
			copy(mw.outbuf[10:], msg.Next(msglen))
			msglen += msglength8
		case msglen > 125:
			mw.outbuf[1] = 126
			mw.outbuf[2] = byte(msglen >> 8)
			mw.outbuf[3] = byte(msglen)
			copy(mw.outbuf[4:], msg.Next(msglen))
			msglen += msglength2
		default:
			mw.outbuf[1] = byte(msglen)
			copy(mw.outbuf[2:], msg.Next(msglen))
		}

		c.Write(mw.outbuf[:msglen+msgheader])
	}
	write_pool.Put(mw)
}
func (c *WSconn) WriteMessage(messageType int, data []byte) error { //通用的输出

	if !isControl(messageType) && !isData(messageType) {
		return errBadWriteOpCode
	}
	if isControl(messageType) && len(data) > maxControlFramePayloadSize {
		return errInvalidControlFrame
	}

	mw := write_pool.Get().(*messageWriter)

	mw.compress = false
	mw.outbuf[0] = byte(messageType)
	mw.writeBuf.Reset()
	if c.IsCompress && len(data) > 512 {
		mw.compress = true
		w := CompressNoContextTakeover(mw, 3)
		w.Write(data)
		w.Close()
	} else {
		mw.writeBuf.Write(data)
	}
	if mw.compress {
		mw.outbuf[0] |= rsv1Bit
	}
	for l := mw.writeBuf.Len(); l > 0; l, mw.outbuf[0] = mw.writeBuf.Len(), continuationFramefinalBit {
		// Check for invalid control frames.

		if l > defaultWriteBufferSize {
			l = defaultWriteBufferSize //当前帧长度，不大于帧大小
			mw.outbuf[0] -= mw.outbuf[0] & finalBit
		}

		msglen := msgheader
		switch {
		case l >= 65536:
			msglen += msglength8

			mw.outbuf[1] = 127
			mw.outbuf[2] = byte(l >> 56)
			mw.outbuf[3] = byte(l >> 48)
			mw.outbuf[4] = byte(l >> 40)
			mw.outbuf[5] = byte(l >> 32)
			mw.outbuf[6] = byte(l >> 24)
			mw.outbuf[7] = byte(l >> 16)
			mw.outbuf[8] = byte(l >> 8)
			mw.outbuf[9] = byte(l)
		case l > 125:
			msglen += msglength2

			mw.outbuf[1] = 126
			mw.outbuf[2] = byte(l >> 8)
			mw.outbuf[3] = byte(l)
		default:
			mw.outbuf[1] = byte(l)
		}
		if !c.IsServer {
			msglen += 4
			copy(mw.outbuf[msglen:], mw.writeBuf.Next(l))
			mw.outbuf[1] |= maskBit
			key := newMaskKey()
			copy(mw.outbuf[msglen-4:msglen], key[:])
			maskBytes(key, 0, mw.outbuf[msglen:msglen+l])
		} else {
			copy(mw.outbuf[msglen:], mw.writeBuf.Next(l))
		}
		// Write the buffers to the connection with best-effort detection of
		// concurrent writes. See the concurrency section in the package
		// documentation for more info.
		c.Write(mw.outbuf[:msglen+l])
	}
	write_pool.Put(mw)
	if messageType == CloseMessage {
		return ErrCloseSent
	}
	return nil
}

var write_pool = sync.Pool{New: func() interface{} {
	return &messageWriter{writeBuf: &tls.MsgBuffer{}, outbuf: make([]byte, defaultWriteBufferSize+msgheader+msglength8+msgmask)}
}}

type messageWriter struct {
	compress bool           // whether next call to flushFrame should set RSV1
	writeBuf *tls.MsgBuffer //:
	outbuf   []byte
}

func (w *messageWriter) Write(p []byte) (int, error) {
	w.writeBuf.Write(p)
	return len(p), nil
}

func (w *messageWriter) WriteString(p string) (int, error) {
	w.writeBuf.WriteString(p)
	return len(p), nil
}

func (w *messageWriter) ReadFrom(r io.Reader) (nn int64, err error) {
	return io.Copy(w.writeBuf, r)
}

func (w *messageWriter) Close() error {
	return nil
}

func isData(frameType int) bool {
	return frameType == TextMessage || frameType == BinaryMessage
}
