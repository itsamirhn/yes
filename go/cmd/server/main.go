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

var serverDecoder = transport.NewDecoder("C") // server receives "C" prefix

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	tokenStr := os.Getenv("SERVER_BOT_TOKEN")
	if tokenStr == "" {
		log.Fatal("SERVER_BOT_TOKEN is required")
	}
	baseURL := envOr("BASE_URL", "https://api.telegram.org/bot")
	upstreamHost := envOr("UPSTREAM_HOST", "127.0.0.1")
	upstreamPort := envOr("UPSTREAM_PORT", "1080")
	webhookURL := os.Getenv("SERVER_WEBHOOK_URL")
	webhookPort := envOr("SERVER_WEBHOOK_PORT", "8443")
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
		log.Fatal("SERVER_BOT_TOKEN: no valid tokens")
	}
	log.Printf("Loaded %d server bot(s)", len(clients))
	log.Printf("File transfers: %v", filesEnabled)
	log.Printf("Upstream: %s:%s", upstreamHost, upstreamPort)

	ctx := context.Background()
	pollClient := clients[0]

	// TelegramPacketConn — server sends "S", receives "C"; chatID set after HELLO
	tgConn := transport.NewTelegramPacketConn(ctx, clients, "", filesEnabled, "S")

	// Wait for HELLO to learn the chatID
	helloCh := make(chan string, 1)

	if webhookURL != "" {
		log.Printf("Mode: webhook")
		go runWebhook(ctx, pollClient, tgConn, helloCh, pollClient.Token, webhookURL, webhookPort, filesEnabled)
	} else {
		log.Printf("Mode: polling")
		go runPolling(ctx, pollClient, tgConn, helloCh, filesEnabled)
	}

	log.Printf("Waiting for client HELLO...")
	chatID := <-helloCh
	log.Printf("Got HELLO from chat %s", chatID)
	tgConn.SetChatID(chatID)

	// Reply READY
	if err := transport.SendControl(ctx, clients[0], chatID, "READY"); err != nil {
		log.Fatalf("Failed to send READY: %v", err)
	}

	// Create KCP listener on top of TelegramPacketConn (nil block = no encryption overhead)
	kcpListener, err := kcp.ServeConn(nil, 0, 0, tgConn)
	if err != nil {
		log.Fatalf("KCP ServeConn error: %v", err)
	}
	log.Printf("KCP listener ready, waiting for client connection...")

	kcpConn, err := kcpListener.AcceptKCP()
	if err != nil {
		log.Fatalf("KCP AcceptKCP error: %v", err)
	}
	log.Printf("KCP session established")

	kcpConn.SetNoDelay(1, 50, 2, 1)
	kcpConn.SetWindowSize(256, 256)
	kcpConn.SetMtu(1400)
	kcpConn.SetStreamMode(true)
	kcpConn.SetACKNoDelay(true)

	// smux server session — high timeouts for Telegram latency
	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = 4 * 1024 * 1024
	smuxConfig.MaxStreamBuffer = 2 * 1024 * 1024
	smuxConfig.KeepAliveInterval = 10 * time.Second
	smuxConfig.KeepAliveTimeout = 300 * time.Second
	session, err := smux.Server(kcpConn, smuxConfig)
	if err != nil {
		log.Fatalf("smux Server error: %v", err)
	}
	log.Printf("smux session ready, accepting streams...")

	upstream := upstreamHost + ":" + upstreamPort
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			log.Printf("smux AcceptStream error: %v", err)
			break
		}
		go handleStream(stream, upstream)
	}
}

func handleStream(stream *smux.Stream, upstream string) {
	defer stream.Close()
	conn, err := net.Dial("tcp", upstream)
	if err != nil {
		log.Printf("Upstream dial error: %v", err)
		return
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(conn, stream)
	}()
	go func() {
		defer wg.Done()
		io.Copy(stream, conn)
	}()
	wg.Wait()
}

func routeMessage(ctx context.Context, msg *tg.Message, pollClient *tg.Client, tgConn *transport.TelegramPacketConn, helloCh chan string, helloSent *bool, filesEnabled bool) {
	if msg == nil {
		return
	}

	// Check for HELLO handshake
	if !*helloSent && msg.Text == "HELLO" {
		chatID := fmt.Sprintf("%d", msg.Chat.ID)
		*helloSent = true
		helloCh <- chatID
		return
	}

	// Try to decode as KCP packet (server receives "C" prefix from client)
	if data := serverDecoder.DecodeKCPPacket(ctx, msg, pollClient, filesEnabled); data != nil {
		addr := &transport.TelegramAddr{ChatID: fmt.Sprintf("%d", msg.Chat.ID)}
		tgConn.Feed(data, addr)
	}
}

func runPolling(ctx context.Context, pollClient *tg.Client, tgConn *transport.TelegramPacketConn, helloCh chan string, filesEnabled bool) {
	var offset *int
	helloSent := false
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
				routeMessage(ctx, msg, pollClient, tgConn, helloCh, &helloSent, filesEnabled)
			}
		}
	}
}

func runWebhook(ctx context.Context, pollClient *tg.Client, tgConn *transport.TelegramPacketConn, helloCh chan string, botToken, webhookURL, webhookPort string, filesEnabled bool) {
	helloSent := false
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
			routeMessage(ctx, msg, pollClient, tgConn, helloCh, &helloSent, filesEnabled)
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
