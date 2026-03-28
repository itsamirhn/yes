package transport

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"yes/internal/base85"
	"yes/internal/mux"
	"yes/internal/tg"
)

// TelegramAddr implements net.Addr for a Telegram chat endpoint.
type TelegramAddr struct {
	ChatID string
}

func (a *TelegramAddr) Network() string { return "telegram" }
func (a *TelegramAddr) String() string  { return "telegram:" + a.ChatID }

// TelegramPacketConn implements net.PacketConn over Telegram messages.
// KCP uses this as its underlying datagram transport.
type TelegramPacketConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	clients     []*tg.Client
	idx         uint64
	enableFiles bool
	sendPrefix  string // "C" for client, "S" for server

	chatMu sync.RWMutex
	chatID string

	incoming chan incomingPacket

	readDeadline  atomic.Value // time.Time
	writeDeadline atomic.Value // time.Time

	closeOnce sync.Once
}

type incomingPacket struct {
	data []byte
	addr net.Addr
}

// NewTelegramPacketConn creates a PacketConn. sendPrefix is "C" for client, "S" for server.
func NewTelegramPacketConn(ctx context.Context, clients []*tg.Client, chatID string, enableFiles bool, sendPrefix string) *TelegramPacketConn {
	ctx2, cancel := context.WithCancel(ctx)
	c := &TelegramPacketConn{
		ctx:         ctx2,
		cancel:      cancel,
		clients:     clients,
		chatID:      chatID,
		incoming:    make(chan incomingPacket, 512),
		enableFiles: enableFiles,
		sendPrefix:  sendPrefix,
	}
	return c
}

func (c *TelegramPacketConn) nextClient() *tg.Client {
	i := atomic.AddUint64(&c.idx, 1)
	return c.clients[i%uint64(len(c.clients))]
}

func (c *TelegramPacketConn) SetChatID(id string) {
	c.chatMu.Lock()
	c.chatID = id
	c.chatMu.Unlock()
}

func (c *TelegramPacketConn) getChatID() string {
	c.chatMu.RLock()
	defer c.chatMu.RUnlock()
	return c.chatID
}

// Feed injects a decoded KCP packet into the conn, called by the polling/webhook handler.
func (c *TelegramPacketConn) Feed(data []byte, from net.Addr) {
	select {
	case c.incoming <- incomingPacket{data: data, addr: from}:
	case <-c.ctx.Done():
	}
}

// ReadFrom implements net.PacketConn. Called by kcp-go's internal recv loop.
func (c *TelegramPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	dl, _ := c.readDeadline.Load().(time.Time)
	var timer <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, nil, os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}

	select {
	case pkt, ok := <-c.incoming:
		if !ok {
			return 0, nil, io.EOF
		}
		n := copy(p, pkt.data)
		return n, pkt.addr, nil
	case <-timer:
		return 0, nil, os.ErrDeadlineExceeded
	case <-c.ctx.Done():
		return 0, nil, c.ctx.Err()
	}
}

// WriteTo implements net.PacketConn. Called by kcp-go to send KCP segments.
func (c *TelegramPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	chatID := c.getChatID()
	if chatID == "" {
		return 0, fmt.Errorf("no chat ID set")
	}
	payload := make([]byte, len(p))
	copy(payload, p)

	compressed := mux.Compress(payload)
	useCompressed := len(compressed) < len(payload)

	var sendPayload []byte
	prefix := c.sendPrefix
	if useCompressed {
		sendPayload = compressed
		prefix = c.sendPrefix + ".z"
	} else {
		sendPayload = payload
	}

	encoded := base85.Encode(sendPayload)
	msg := prefix + " " + string(encoded)

	if len(msg) <= 4096 {
		err := c.nextClient().SendMessage(c.ctx, chatID, msg)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}

	if c.enableFiles {
		fileName := c.sendPrefix + ".bin"
		if useCompressed {
			fileName = c.sendPrefix + ".z.bin"
		}
		err := c.nextClient().SendDocument(c.ctx, chatID, sendPayload, fileName)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}

	return 0, fmt.Errorf("packet too large for text (%d chars) and files disabled", len(msg))
}

func (c *TelegramPacketConn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		close(c.incoming)
	})
	return nil
}

func (c *TelegramPacketConn) LocalAddr() net.Addr {
	return &TelegramAddr{ChatID: c.getChatID()}
}

func (c *TelegramPacketConn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	c.writeDeadline.Store(t)
	return nil
}

func (c *TelegramPacketConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	return nil
}

func (c *TelegramPacketConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.Store(t)
	return nil
}

// SendControl sends a plain text control message (HELLO/READY) via Telegram.
func SendControl(ctx context.Context, client *tg.Client, chatID, text string) error {
	log.Printf("Sending control: %s", text)
	return client.SendMessage(ctx, chatID, text)
}
