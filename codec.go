// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package gnet

// CRLFByte represents a byte of CRLF.
var CRLFByte = byte('\n')

type (
	// ICodec is the interface of gnet codec.
	ICodec interface {
		// Encode encodes frames upon server responses into TCP stream.
		Encode(c Conn, buf []byte) ([]byte, error)
		// Decode decodes frames from TCP stream via specific implementation.
		Decode(c Conn) ([]byte, error)
	}

	// BuiltInFrameCodec is the built-in codec which will be assigned to gnet server when customized codec is not set up.
	BuiltInFrameCodec struct {
	}
)

// Encode ...
func (cc *BuiltInFrameCodec) Encode(c Conn, buf []byte) ([]byte, error) {
	return buf, nil
}

// Decode ...
func (cc *BuiltInFrameCodec) Decode(c Conn) ([]byte, error) {
	buf := c.Read()
	if len(buf) == 0 {
		return nil, nil
	}
	c.ResetBuffer()
	return buf, nil
}
