package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"yes/internal/tg"
	"yes/internal/transport"

	kcp "github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

var clientDecoder = transport.NewDecoder("S") // client receives "S" prefix

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	tokenStr := os.Getenv("CLIENT_BOT_TOKEN")
	if tokenStr == "" {
		log.Fatal("CLIENT_BOT_TOKEN is required")
	}
	chatID := os.Getenv("CHAT_ID")
	if chatID == "" {
		log.Fatal("CHAT_ID is required")
	}
	baseURL := envOr("BASE_URL", "https://api.telegram.org/bot")
	listenHost := envOr("LISTEN_HOST", "0.0.0.0")
	listenPort := envOr("LISTEN_PORT", "1080")
	webhookURL := os.Getenv("CLIENT_WEBHOOK_URL")
	webhookPort := envOr("CLIENT_WEBHOOK_PORT", "8443")
	enableFiles := strings.ToLower(os.Getenv("ENABLE_FILES"))
	filesEnabled := enableFiles == "1" || enableFiles == "true" || enableFiles == "yes"

	var clients []*tg.Client
	for _, tok := range strings.Split(tokenStr, ",") {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			clients = append(clients, tg.NewClient(tok, baseURL))
		}
	}
	if len(clients) == 0 {
		log.Fatal("CLIENT_BOT_TOKEN: no valid tokens")
	}
	log.Printf("Loaded %d client bot(s)", len(clients))
	log.Printf("File transfers: %v", filesEnabled)

	ctx := context.Background()

	for {
		err := runSession(ctx, clients, chatID, listenHost, listenPort, webhookURL, webhookPort, filesEnabled)
		log.Printf("Session ended: %v, reconnecting in 5s", err)
		time.Sleep(5 * time.Second)
	}
}

func runSession(ctx context.Context, clients []*tg.Client, chatID, listenHost, listenPort, webhookURL, webhookPort string, filesEnabled bool) error {
	// Create TelegramPacketConn — client sends "C", receives "S"
	tgConn := transport.NewTelegramPacketConn(ctx, clients, chatID, filesEnabled, "C")
	defer tgConn.Close()

	pollClient := clients[0]

	// Start receiver (polling/webhook) — must be running before we send HELLO
	readyCh := make(chan struct{}, 1)

	if webhookURL != "" {
		log.Printf("Mode: webhook")
		go runWebhook(ctx, pollClient, tgConn, readyCh, pollClient.Token, webhookURL, webhookPort, filesEnabled)
	} else {
		log.Printf("Mode: polling")
		go runPolling(ctx, pollClient, tgConn, readyCh, filesEnabled)
	}

	// Send HELLO handshake
	log.Printf("Sending HELLO to chat %s", chatID)
	if err := transport.SendControl(ctx, clients[0], chatID, "HELLO"); err != nil {
		return fmt.Errorf("send HELLO: %w", err)
	}

	// Wait for READY
	log.Printf("Waiting for READY...")
	select {
	case <-readyCh:
		log.Printf("Got READY from server")
	case <-time.After(60 * time.Second):
		return fmt.Errorf("timeout waiting for READY")
	}

	// Create KCP connection (nil block = no encryption overhead)
	kcpConn, err := kcp.NewConn3(1, nil, nil, 0, 0, tgConn)
	if err != nil {
		return fmt.Errorf("KCP NewConn3: %w", err)
	}
	defer kcpConn.Close()
	log.Printf("KCP session established")

	kcpConn.SetNoDelay(1, 50, 2, 1)
	kcpConn.SetWindowSize(256, 256)
	kcpConn.SetMtu(1400)
	kcpConn.SetStreamMode(true)
	kcpConn.SetACKNoDelay(true)

	// smux client session — high timeouts for Telegram latency
	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = 4 * 1024 * 1024
	smuxConfig.MaxStreamBuffer = 2 * 1024 * 1024
	smuxConfig.KeepAliveInterval = 10 * time.Second
	smuxConfig.KeepAliveTimeout = 300 * time.Second
	session, err := smux.Client(kcpConn, smuxConfig)
	if err != nil {
		return fmt.Errorf("smux Client: %w", err)
	}
	defer session.Close()
	log.Printf("smux session ready")

	// TCP listener
	ln, err := net.Listen("tcp", listenHost+":"+listenPort)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	log.Printf("Tunnel listening on %s:%s", listenHost, listenPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go handleClient(conn, session)
	}
}

func handleClient(conn net.Conn, session *smux.Session) {
	defer conn.Close()
	stream, err := session.OpenStream()
	if err != nil {
		log.Printf("OpenStream error: %v", err)
		return
	}
	defer stream.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(stream, conn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn, stream)
	}()
	wg.Wait()
}

func routeMessage(ctx context.Context, msg *tg.Message, pollClient *tg.Client, tgConn *transport.TelegramPacketConn, readyCh chan struct{}, readySent *bool, filesEnabled bool) {
	if msg == nil {
		return
	}

	// Check for READY handshake
	if !*readySent && msg.Text == "READY" {
		*readySent = true
		select {
		case readyCh <- struct{}{}:
		default:
		}
		return
	}

	// Try to decode as KCP packet (client receives "S" prefix from server)
	if data := clientDecoder.DecodeKCPPacket(ctx, msg, pollClient, filesEnabled); data != nil {
		addr := &transport.TelegramAddr{ChatID: fmt.Sprintf("%d", msg.Chat.ID)}
		tgConn.Feed(data, addr)
	}
}

func runPolling(ctx context.Context, pollClient *tg.Client, tgConn *transport.TelegramPacketConn, readyCh chan struct{}, filesEnabled bool) {
	var offset *int
	readySent := false
	for {
		updates, err := pollClient.GetUpdates(ctx, offset, 10)
		if err != nil {
			log.Printf("GetUpdates error: %v", err)
			continue
		}
		for _, u := range updates {
			newOff := u.UpdateID + 1
			offset = &newOff
			msg := u.ChannelPost
			if msg == nil {
				msg = u.Message
			}
			if msg != nil {
				routeMessage(ctx, msg, pollClient, tgConn, readyCh, &readySent, filesEnabled)
			}
		}
	}
}

func runWebhook(ctx context.Context, pollClient *tg.Client, tgConn *transport.TelegramPacketConn, readyCh chan struct{}, botToken, webhookURL, webhookPort string, filesEnabled bool) {
	readySent := false
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/"+botToken, func(w http.ResponseWriter, r *http.Request) {
		var update tg.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		msg := update.ChannelPost
		if msg == nil {
			msg = update.Message
		}
		if msg != nil {
			routeMessage(ctx, msg, pollClient, tgConn, readyCh, &readySent, filesEnabled)
		}
		w.Write([]byte("ok"))
	})

	addr := "0.0.0.0:" + webhookPort
	log.Printf("Webhook server listening on %s", addr)

	err := pollClient.SetWebhook(ctx, webhookURL+"/"+botToken)
	if err != nil {
		log.Printf("SetWebhook error: %v", err)
	} else {
		log.Printf("Webhook set to %s/%s", webhookURL, botToken)
	}

	log.Fatal(http.ListenAndServe(addr, httpMux))
}
