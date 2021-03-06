// Copyright 2018 The go-zeromq Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package zmtp implements the ZeroMQ Message Transport Protocol as defined
// in https://rfc.zeromq.org/spec:23/ZMTP/.
package zmtp

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"

	"github.com/pkg/errors"
)

// Conn implements the ZeroMQ Message Transport Protocol as defined
// in https://rfc.zeromq.org/spec:23/ZMTP/.
type Conn struct {
	typ    SocketType
	id     SocketIdentity
	rw     io.ReadWriter
	sec    Security
	server bool
	peer   struct {
		server bool
		md     map[string]string
	}
}

// Open opens a ZMTP connection over rw with the given security, socket type and identity.
// Open performs a complete ZMTP handshake.
func Open(rw io.ReadWriter, sec Security, sockType SocketType, sockID SocketIdentity, server bool) (*Conn, error) {
	if rw == nil {
		return nil, errors.Errorf("zmtp: invalid nil read-writer")
	}

	conn := &Conn{
		typ:    sockType,
		id:     sockID,
		rw:     rw,
		sec:    sec,
		server: server,
	}

	err := conn.init(sec, nil)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// init performs a ZMTP handshake over an io.ReadWriter
func (conn *Conn) init(sec Security, md map[string]string) error {
	var err error

	err = conn.greet(conn.server)
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not exchange greetings")
	}

	err = conn.sec.Handshake()
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not perform security handshake")
	}

	err = conn.sendMD(md)
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not send metadata to peer")
	}

	conn.peer.md, err = conn.recvMD()
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not recv metadata from peer")
	}

	// FIXME(sbinet): if security mechanism does not define a client/server
	// topology, enforce that p.server == p.peer.server == 0
	// as per:
	//  https://rfc.zeromq.org/spec:23/ZMTP/#topology

	return nil
}

func (conn *Conn) greet(server bool) error {
	var err error
	send := greeting{Version: defaultVersion}
	send.Sig.Header = sigHeader
	send.Sig.Footer = sigFooter
	kind := string(conn.sec.Type())
	if len(kind) > len(send.Mechanism) {
		return errSecMech
	}
	copy(send.Mechanism[:], kind)

	err = send.write(conn.rw)
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not send greeting")
	}

	var recv greeting
	err = recv.read(conn.rw)
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not recv greeting")
	}

	peerKind := asString(recv.Mechanism[:])
	if peerKind != kind {
		return errBadSec
	}

	conn.peer.server, err = asBool(recv.Server)
	if err != nil {
		return errors.Wrapf(err, "zmtp: could not get peer server flag")
	}

	return nil
}

func (c *Conn) sendMD(appMD map[string]string) error {
	buf := new(bytes.Buffer)
	keys := make(map[string]struct{})

	for k, v := range appMD {
		if len(k) == 0 {
			return errEmptyAppMDKey
		}

		key := strings.ToLower(k)
		if _, dup := keys[key]; dup {
			return errDupAppMDKey
		}

		keys[key] = struct{}{}
		if _, err := io.Copy(buf, metaData{k: "X-" + key, v: v}); err != nil {
			return err
		}
	}

	if _, err := io.Copy(buf, metaData{k: sysSockType, v: string(c.typ)}); err != nil {
		return err
	}
	if _, err := io.Copy(buf, metaData{k: sysSockID, v: c.id.String()}); err != nil {
		return err
	}
	return c.SendCmd(cmdReady, buf.Bytes())
}

func (c *Conn) recvMD() (map[string]string, error) {
	isCommand, body, err := c.read()
	if err != nil {
		return nil, err
	}

	if !isCommand {
		return nil, errBadFrame
	}

	var cmd command
	err = cmd.unmarshalZMTP(body)
	if err != nil {
		return nil, err
	}

	if cmd.Name != cmdReady {
		return nil, errBadCmd
	}

	sysMetadata := make(map[string]string)
	appMetadata := make(map[string]string)
	i := 0
	for i < len(cmd.Body) {
		var kv metaData
		n, err := kv.Write(cmd.Body[i:])
		if err != nil {
			return nil, err
		}
		i += n

		name := strings.Title(kv.k)
		if strings.HasPrefix(name, "X-") {
			appMetadata[name[2:]] = kv.v
		} else {
			sysMetadata[name] = kv.v
		}
	}

	peer := SocketType(sysMetadata[sysSockType])
	if !peer.IsCompatible(c.typ) {
		return nil, errors.Errorf("zmtp: peer=%q not compatible with %q", peer, c.typ)
	}
	return appMetadata, nil
}

// SendCmd sends a ZMTP command over the wire.
func (c *Conn) SendCmd(name string, body []byte) error {
	cmd := command{Name: name, Body: body}
	buf, err := cmd.marshalZMTP()
	if err != nil {
		return err
	}
	return c.send(true, buf, 0)
}

// SendMsg sends a ZMTP message over the wire.
func (c *Conn) SendMsg(body []byte) error {
	return c.send(false, body, 0)
}

// RecvMsg receives a ZMTP message from the wire.
func (c *Conn) RecvMsg() ([]byte, error) {
	isCmd, body, err := c.read()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if !isCmd {
		return body, nil
	}

	var cmd command
	err = cmd.unmarshalZMTP(body)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// FIXME(sbinet)
	switch cmd.Name {
	case cmdPing:
		panic("got PING")
	}

	return cmd.Body, nil
}

func (c *Conn) send(isCommand bool, body []byte, flag byte) error {
	// Long flag
	size := len(body)
	isLong := size > 255
	if isLong {
		flag ^= isLongBitFlag
	}

	if isCommand {
		flag ^= isCommandBitFlag
	}

	// Write out the message itself
	if _, err := c.rw.Write([]byte{flag}); err != nil {
		return err
	}

	if isLong {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(size))
		if _, err := c.rw.Write(buf[:]); err != nil {
			return err
		}
	} else {
		if _, err := c.rw.Write([]byte{uint8(size)}); err != nil {
			return err
		}
	}

	if _, err := c.sec.Encrypt(c.rw, body); err != nil {
		return err
	}

	return nil
}

// read returns the isCommand flag, the body of the message, and optionally an error
func (c *Conn) read() (bool, []byte, error) {
	var header [2]byte

	// Read out the header
	_, err := io.ReadFull(c.rw, header[:])
	if err != nil {
		return false, nil, err
	}

	fl := flag(header[0])

	// FIXME(sbinet): implement MORE commands
	if fl.hasMore() {
		return false, nil, errMoreCmd
	}

	// Determine the actual length of the body
	size := uint64(header[1])
	if fl.isLong() {
		var longHeader [8]byte
		// We read 2 bytes of the header already
		// In case of a long message, the length is bytes 2-8 of the header
		// We already have the first byte, so assign it, and then read the rest
		longHeader[0] = header[1]

		_, err = io.ReadFull(c.rw, longHeader[1:])
		if err != nil {
			return false, nil, err
		}

		size = binary.BigEndian.Uint64(longHeader[:])
	}

	if size > uint64(maxInt64) {
		return false, nil, errOverflow
	}

	body := make([]byte, size)
	_, err = io.ReadFull(c.rw, body)
	if err != nil {
		return false, nil, err
	}

	// fast path for NULL security: we bypass the bytes.Buffer allocation.
	if c.sec.Type() == NullSecurity {
		return fl.isCommand(), body, nil
	}

	buf := new(bytes.Buffer)
	if _, err := c.sec.Decrypt(buf, body); err != nil {
		return false, nil, err
	}

	return fl.isCommand(), buf.Bytes(), nil
}
