package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"yes/internal/base85"
	"yes/internal/mux"
	"yes/internal/tg"
)

var rrIdx uint64

func rrClient(clients []*tg.Client) *tg.Client {
	i := atomic.AddUint64(&rrIdx, 1)
	return clients[i%uint64(len(clients))]
}

var (
	reOK     = regexp.MustCompile(`^OK (\S+) (\S+)$`)
	reRECV   = regexp.MustCompile(`^RECV (\S+)$`)
	reRECVZ  = regexp.MustCompile(`^RECV\.z (\S+)$`)
	reCLOSED = regexp.MustCompile(`^CLOSED (\S+)$`)
)

type connectResult struct {
	streamID string
	buf      *mux.StreamBuffer
}

var (
	connectsMu sync.Mutex
	connects   = map[string]chan connectResult{}

	streamsMu sync.Mutex
	streams   = map[string]*mux.StreamBuffer{}
)

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

	ctx := context.Background()
	sq := mux.NewSendQueue(clients, chatID, "SEND", filesEnabled)
	sq.Start(ctx)

	chunkSize := 3100
	if filesEnabled {
		chunkSize = 4096
	}

	log.Printf("Starting tunnel client on %s:%s", listenHost, listenPort)
	log.Printf("File transfers: %v", filesEnabled)

	// Use first client for receiving (it sees all channel messages)
	pollClient := clients[0]

	if webhookURL != "" {
		log.Printf("Mode: webhook")
		go runWebhook(ctx, pollClient, pollClient.Token, webhookURL, webhookPort, filesEnabled)
	} else {
		log.Printf("Mode: polling")
		go runPolling(ctx, pollClient, filesEnabled)
	}

	ln, err := net.Listen("tcp", listenHost+":"+listenPort)
	if err != nil {
		log.Fatalf("Listen failed: %v", err)
	}
	log.Printf("Tunnel listening on %s:%s", listenHost, listenPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go handleClient(ctx, conn, clients, chatID, sq, chunkSize)
	}
}

func handleClient(ctx context.Context, conn net.Conn, clients []*tg.Client, chatID string, sq *mux.SendQueue, chunkSize int) {
	defer conn.Close()

	buf, streamID, err := openConnection(ctx, clients, chatID)
	if err != nil {
		log.Printf("openConnection error: %v", err)
		return
	}

	// Register stream
	streamsMu.Lock()
	streams[streamID] = buf
	streamsMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)

	// client TCP → sendqueue
	go func() {
		defer wg.Done()
		tmp := make([]byte, chunkSize)
		for {
			n, err := conn.Read(tmp)
			if n > 0 {
				data := make([]byte, n)
				copy(data, tmp[:n])
				sq.Push(streamID, data)
			}
			if err != nil {
				break
			}
		}
		rrClient(clients).SendMessage(ctx, chatID, "CLOSE "+streamID)
	}()

	// streambuffer → client TCP
	go func() {
		defer wg.Done()
		for {
			data, err := buf.Read(chunkSize)
			if err != nil {
				break
			}
			_, werr := conn.Write(data)
			if werr != nil {
				break
			}
		}
		conn.Close()
	}()

	wg.Wait()

	streamsMu.Lock()
	delete(streams, streamID)
	streamsMu.Unlock()
}

func openConnection(ctx context.Context, clients []*tg.Client, chatID string) (*mux.StreamBuffer, string, error) {
	requestID := newRequestID()

	ch := make(chan connectResult, 1)
	connectsMu.Lock()
	connects[requestID] = ch
	connectsMu.Unlock()

	err := rrClient(clients).SendMessage(ctx, chatID, "CONNECT "+requestID)
	if err != nil {
		connectsMu.Lock()
		delete(connects, requestID)
		connectsMu.Unlock()
		return nil, "", err
	}

	result := <-ch
	connectsMu.Lock()
	delete(connects, requestID)
	connectsMu.Unlock()

	return result.buf, result.streamID, nil
}

func newRequestID() string {
	f, _ := os.Open("/dev/urandom")
	b := make([]byte, 16)
	f.Read(b)
	f.Close()
	return fmt.Sprintf("%x", b)
}

func handleMessage(ctx context.Context, msg *tg.Message, pollClient *tg.Client, filesEnabled bool) {
	if msg == nil {
		return
	}

	if msg.Text != "" {
		// OK
		if m := reOK.FindStringSubmatch(msg.Text); m != nil {
			requestID, streamID := m[1], m[2]
			log.Printf("OK for request %s, stream %s", requestID, streamID)
			connectsMu.Lock()
			ch, ok := connects[requestID]
			connectsMu.Unlock()
			if ok {
				buf := mux.NewStreamBuffer()
				streamsMu.Lock()
				streams[streamID] = buf
				streamsMu.Unlock()
				ch <- connectResult{streamID: streamID, buf: buf}
			}
			return
		}

		// RECV.z text (compressed)
		if m := reRECVZ.FindStringSubmatch(msg.Text); m != nil {
			compressed, err := base85.Decode([]byte(m[1]))
			if err != nil {
				log.Printf("base85 decode error: %v", err)
				return
			}
			raw, err := mux.Decompress(compressed)
			if err != nil {
				log.Printf("decompress error: %v", err)
				return
			}
			dispatchFrames(raw)
			return
		}

		// RECV text (uncompressed)
		if m := reRECV.FindStringSubmatch(msg.Text); m != nil {
			raw, err := base85.Decode([]byte(m[1]))
			if err != nil {
				log.Printf("base85 decode error: %v", err)
				return
			}
			dispatchFrames(raw)
			return
		}

		// CLOSED
		if m := reCLOSED.FindStringSubmatch(msg.Text); m != nil {
			streamID := m[1]
			log.Printf("Stream %s closed by server", streamID)
			streamsMu.Lock()
			buf, ok := streams[streamID]
			if ok {
				delete(streams, streamID)
			}
			streamsMu.Unlock()
			if ok {
				buf.Close()
			}
			return
		}
	}

	// RECV.bin / RECV.z.bin file
	if filesEnabled && msg.Document != nil {
		fn := msg.Document.FileName
		if fn == "RECV.bin" || fn == "RECV.z.bin" {
			raw, err := pollClient.DownloadDocument(ctx, msg.Document.FileID)
			if err != nil {
				log.Printf("Download document error: %v", err)
				return
			}
			if fn == "RECV.z.bin" {
				raw, err = mux.Decompress(raw)
				if err != nil {
					log.Printf("decompress error: %v", err)
					return
				}
			}
			dispatchFrames(raw)
			return
		}
	}
}

func dispatchFrames(raw []byte) {
	for _, f := range mux.UnpackFrames(raw) {
		streamsMu.Lock()
		buf, ok := streams[f.StreamID]
		streamsMu.Unlock()
		if ok {
			buf.Write(f.Data)
		}
	}
}

func runPolling(ctx context.Context, pollClient *tg.Client, filesEnabled bool) {
	var offset *int
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
				handleMessage(ctx, msg, pollClient, filesEnabled)
			}
		}
	}
}

func runWebhook(ctx context.Context, pollClient *tg.Client, botToken, webhookURL, webhookPort string, filesEnabled bool) {
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
			handleMessage(ctx, msg, pollClient, filesEnabled)
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
