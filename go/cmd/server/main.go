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
	"regexp"
	"strings"
	"sync"
	"yes/internal/base85"
	"yes/internal/mux"
	"yes/internal/tg"
)

var (
	reCONNECT = regexp.MustCompile(`^CONNECT (\S+)$`)
	reSEND    = regexp.MustCompile(`^SEND (\S+)$`)
	reCLOSE   = regexp.MustCompile(`^CLOSE (\S+)$`)
)

var (
	connsMu sync.Mutex
	conns   = map[string]net.Conn{}

	// Track seen request IDs to avoid duplicate handling
	seenMu sync.Mutex
	seen   = map[string]bool{}
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	botToken := os.Getenv("SERVER_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("SERVER_BOT_TOKEN is required")
	}
	baseURL := envOr("BASE_URL", "https://api.telegram.org/bot")
	upstreamHost := envOr("UPSTREAM_HOST", "127.0.0.1")
	upstreamPort := envOr("UPSTREAM_PORT", "1080")
	webhookURL := os.Getenv("SERVER_WEBHOOK_URL")
	webhookPort := envOr("SERVER_WEBHOOK_PORT", "8443")
	enableFiles := strings.ToLower(os.Getenv("ENABLE_FILES"))
	filesEnabled := enableFiles == "1" || enableFiles == "true" || enableFiles == "yes"

	ctx := context.Background()
	client := tg.NewClient(botToken, baseURL)
	sq := mux.NewSendQueue(client, "", "RECV", filesEnabled)
	sq.Start(ctx)

	chunkSize := 3100
	if filesEnabled {
		chunkSize = 4096
	}

	log.Printf("File transfers: %v", filesEnabled)
	log.Printf("Upstream: %s:%s", upstreamHost, upstreamPort)

	if webhookURL != "" {
		log.Printf("Mode: webhook")
		runWebhook(ctx, client, sq, botToken, webhookURL, webhookPort, upstreamHost, upstreamPort, filesEnabled, chunkSize)
	} else {
		log.Printf("Mode: polling")
		runPolling(ctx, client, sq, upstreamHost, upstreamPort, filesEnabled, chunkSize)
	}
}

func handleMessage(ctx context.Context, msg *tg.Message, client *tg.Client, sq *mux.SendQueue, upstreamHost, upstreamPort string, filesEnabled bool, chunkSize int) {
	if msg == nil {
		return
	}

	chatID := fmt.Sprintf("%d", msg.Chat.ID)

	if msg.Text != "" {
		// CONNECT
		if m := reCONNECT.FindStringSubmatch(msg.Text); m != nil {
			requestID := m[1]

			seenMu.Lock()
			if seen[requestID] {
				seenMu.Unlock()
				return
			}
			seen[requestID] = true
			seenMu.Unlock()

			log.Printf("New tunnel request %s, connecting to %s:%s", requestID, upstreamHost, upstreamPort)
			streamID := newStreamID()

			conn, err := net.Dial("tcp", upstreamHost+":"+upstreamPort)
			if err != nil {
				log.Printf("Failed to connect to upstream: %v", err)
				return
			}

			connsMu.Lock()
			conns[streamID] = conn
			connsMu.Unlock()

			sq.SetChatID(chatID)

			// Reader goroutine: upstream → sendqueue
			go func() {
				buf := make([]byte, chunkSize)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						data := make([]byte, n)
						copy(data, buf[:n])
						sq.Push(streamID, data)
					}
					if err != nil {
						if err != io.EOF {
							log.Printf("Upstream read error for %s: %v", streamID, err)
						}
						log.Printf("Upstream closed for stream %s", streamID)
						connsMu.Lock()
						delete(conns, streamID)
						connsMu.Unlock()
						client.SendMessage(ctx, chatID, "CLOSED "+streamID)
						return
					}
				}
			}()

			client.SendMessage(ctx, chatID, fmt.Sprintf("OK %s %s", requestID, streamID))
			return
		}

		// SEND text
		if m := reSEND.FindStringSubmatch(msg.Text); m != nil {
			raw, err := base85.Decode([]byte(m[1]))
			if err != nil {
				log.Printf("base85 decode error: %v", err)
				return
			}
			writeToUpstream(raw)
			return
		}

		// CLOSE
		if m := reCLOSE.FindStringSubmatch(msg.Text); m != nil {
			streamID := m[1]
			log.Printf("Closing stream %s", streamID)
			connsMu.Lock()
			conn, ok := conns[streamID]
			if ok {
				delete(conns, streamID)
			}
			connsMu.Unlock()
			if ok {
				conn.Close()
				client.SendMessage(ctx, chatID, "CLOSED "+streamID)
			}
			return
		}
	}

	// SEND.bin file
	if filesEnabled && msg.Document != nil && msg.Document.FileName == "SEND.bin" {
		raw, err := client.DownloadDocument(ctx, msg.Document.FileID)
		if err != nil {
			log.Printf("Download document error: %v", err)
			return
		}
		writeToUpstream(raw)
		return
	}
}

func writeToUpstream(raw []byte) {
	for _, f := range mux.UnpackFrames(raw) {
		connsMu.Lock()
		conn, ok := conns[f.StreamID]
		connsMu.Unlock()
		if ok {
			conn.Write(f.Data)
		}
	}
}

func newStreamID() string {
	f, _ := os.Open("/dev/urandom")
	b := make([]byte, 16)
	f.Read(b)
	f.Close()
	return fmt.Sprintf("%x", b)
}

func runPolling(ctx context.Context, client *tg.Client, sq *mux.SendQueue, upstreamHost, upstreamPort string, filesEnabled bool, chunkSize int) {
	var offset *int
	for {
		updates, err := client.GetUpdates(ctx, offset, 10)
		if err != nil {
			log.Printf("GetUpdates error: %v", err)
			continue
		}
		for _, u := range updates {
			newOff := u.UpdateID + 1
			offset = &newOff
			if u.ChannelPost != nil {
				handleMessage(ctx, u.ChannelPost, client, sq, upstreamHost, upstreamPort, filesEnabled, chunkSize)
			}
		}
	}
}

func runWebhook(ctx context.Context, client *tg.Client, sq *mux.SendQueue, botToken, webhookURL, webhookPort, upstreamHost, upstreamPort string, filesEnabled bool, chunkSize int) {
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/"+botToken, func(w http.ResponseWriter, r *http.Request) {
		var update tg.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if update.ChannelPost != nil {
			handleMessage(ctx, update.ChannelPost, client, sq, upstreamHost, upstreamPort, filesEnabled, chunkSize)
		}
		w.Write([]byte("ok"))
	})

	addr := "0.0.0.0:" + webhookPort
	log.Printf("Webhook server listening on %s", addr)

	err := client.SetWebhook(ctx, webhookURL+"/"+botToken)
	if err != nil {
		log.Printf("SetWebhook error: %v", err)
	} else {
		log.Printf("Webhook set to %s/%s", webhookURL, botToken)
	}

	log.Fatal(http.ListenAndServe(addr, httpMux))
}
