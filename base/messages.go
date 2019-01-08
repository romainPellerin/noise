package base

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"github.com/romainPellerin/noise/log"
	"github.com/romainPellerin/noise/protocol"
	"github.com/pkg/errors"
	"io"
	"net"
)

var _ protocol.MessageAdapter = (*MessageAdapter)(nil)

type MessageAdapter struct {
	conn              net.Conn
	finalizerNotifier chan struct{}
	metadata          map[string]string
}

func NewMessageAdapterPassive(connAdapter protocol.ConnectionAdapter, conn net.Conn, local []byte, localAddr string) (*MessageAdapter, error) {

	if len(local) > 255 {
		return nil, errors.New("local or remote id too long")
	}
	byteBuf := make([]byte, 1)

	remote := make([]byte, len(local))

	_, err := io.ReadFull(conn, remote)
	if err != nil {
		conn.Close()
		return nil, err
	}

	_, err = conn.Write(local)
	if err != nil {
		conn.Close()
		return nil, err
	}

	_, err = io.ReadFull(conn, byteBuf)
	if err != nil {
		conn.Close()
		return nil, err
	}

	localLen := int(byteBuf[0])
	if localLen > 0 {
		localBytes := make([]byte, localLen)
		_, err = io.ReadFull(conn, localBytes)
		if err != nil {
			conn.Close()
			return nil, err
		}
		localAddr = string(localBytes)
	}

	_, err = io.ReadFull(conn, byteBuf)
	if err != nil {
		conn.Close()
		return nil, err
	}

	var remoteAddr string
	remoteLen := int(byteBuf[0])
	if remoteLen > 0 {
		remoteBytes := make([]byte, remoteLen)
		_, err = io.ReadFull(conn, remoteBytes)
		if err != nil {
			conn.Close()
			return nil, err
		}
		remoteAddr = string(remoteBytes)
	}

	adapter := &MessageAdapter{
		conn:              conn,
		finalizerNotifier: make(chan struct{}),
		metadata: map[string]string{
			"local":      string(local),
			"remote":     string(remote),
			"localAddr":  localAddr,
			"remoteAddr": remoteAddr,
		},
	}

	return adapter, nil
}

func NewMessageAdapterActive(connAdapter protocol.ConnectionAdapter, conn net.Conn, local, remote []byte, localAddr, remoteAddr string) (*MessageAdapter, error) {

	if len(local) > 255 || len(remote) > 255 {
		return nil, errors.New("local or remote id too long")
	}

	if len(remoteAddr) > 255 {
		conn.Close()
		return nil, errors.Errorf("remote address is too long")
	}

	if len(localAddr) > 255 {
		conn.Close()
		return nil, errors.Errorf("local address is too long")
	}

	_, err := conn.Write(local)
	if err != nil {
		conn.Close()
		return nil, err
	}
	recvRemote := make([]byte, len(local))
	_, err = io.ReadFull(conn, recvRemote)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !bytes.Equal(recvRemote, remote) {
		conn.Close()
		return nil, errors.Errorf("inconsistent remotes %s and %s", hex.EncodeToString(recvRemote), hex.EncodeToString(remote))
	}

	_, err = conn.Write(append([]byte{byte(len(remoteAddr))}, []byte(remoteAddr)...))
	if err != nil {
		conn.Close()
		return nil, err
	}

	_, err = conn.Write(append([]byte{byte(len(localAddr))}, []byte(localAddr)...))
	if err != nil {
		conn.Close()
		return nil, err
	}

	adapter := &MessageAdapter{
		conn:              conn,
		finalizerNotifier: make(chan struct{}),
		metadata: map[string]string{
			"local":      string(local),
			"remote":     string(remote),
			"localAddr":  localAddr,
			"remoteAddr": remoteAddr,
		},
	}

	return adapter, nil
}

func (a *MessageAdapter) Close() {
	a.conn.Close()
	close(a.finalizerNotifier)
}

func (a *MessageAdapter) RemoteID() []byte {
	return []byte(a.metadata["remote"])
}

func (a *MessageAdapter) Metadata() map[string]string {
	return a.metadata
}

func (a *MessageAdapter) SendMessage(c *protocol.Controller, message []byte) error {
	lenBuf := make([]byte, 16)
	n := binary.PutUvarint(lenBuf, uint64(len(message)))
	_, err := a.conn.Write(lenBuf[:n])
	if err != nil {
		return err
	}
	_, err = a.conn.Write(message)
	if err != nil {
		return err
	}
	return nil
}

func (a *MessageAdapter) OnRecvMessage(c *protocol.Controller, callback protocol.RecvMessageCallback) {
	go runRecvWorker(a.finalizerNotifier, a.conn, callback)
}

func runRecvWorker(finalizerNotifier chan struct{}, conn net.Conn, callback protocol.RecvMessageCallback) {
	reader := bufio.NewReader(conn)

	for {
		// conn should also be closed as soon as gcFinalize() is called
		// so we do not need to check finalizerNotifier?
		n, err := binary.ReadUvarint(reader)
		if err != nil {
			break
		}

		// not so accurate since the message header takes a few bytes;
		// but it works just fine here.
		if n > protocol.MaxPayloadLen {
			log.Error().Msg("message too long")
			break
		}

		buf := make([]byte, int(n))
		_, err = io.ReadFull(reader, buf)
		if err != nil {
			break
		}

		callback(context.Background(), buf)
	}

	callback(context.Background(), nil)
}
