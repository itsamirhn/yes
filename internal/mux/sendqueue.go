package mux

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"yes/internal/base85"
	"yes/internal/tg"
)

const TextMaxRaw = 3200

type SendQueue struct {
	ch          chan Frame
	clients     []*tg.Client
	idx         uint64
	chatID      string
	mu          sync.Mutex
	prefix      string
	enableFiles bool
}

func NewSendQueue(clients []*tg.Client, chatID, prefix string, enableFiles bool) *SendQueue {
	return &SendQueue{
		ch:          make(chan Frame, 4096),
		clients:     clients,
		chatID:      chatID,
		prefix:      prefix,
		enableFiles: enableFiles,
	}
}

func (q *SendQueue) nextClient() *tg.Client {
	i := atomic.AddUint64(&q.idx, 1)
	return q.clients[i%uint64(len(q.clients))]
}

func (q *SendQueue) SetChatID(id string) {
	q.mu.Lock()
	q.chatID = id
	q.mu.Unlock()
}

func (q *SendQueue) getChatID() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.chatID
}

func (q *SendQueue) Push(streamID string, data []byte) {
	q.ch <- Frame{StreamID: streamID, Data: data}
}

func (q *SendQueue) Start(ctx context.Context) {
	go q.flushLoop(ctx)
}

func (q *SendQueue) flushLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case first := <-q.ch:
			frames := []Frame{first}
			// Drain non-blocking
		drain:
			for {
				select {
				case f := <-q.ch:
					frames = append(frames, f)
				default:
					break drain
				}
			}
			q.sendBatch(ctx, frames)
		}
	}
}

func (q *SendQueue) sendBatch(ctx context.Context, frames []Frame) {
	chatID := q.getChatID()
	for chatID == "" {
		log.Printf("SendQueue: waiting for chat ID (%d frames pending)", len(frames))
		time.Sleep(500 * time.Millisecond)
		chatID = q.getChatID()
	}

	raw := PackFrames(frames)
	compressed := Compress(raw)

	// Use compressed if it's actually smaller
	payload := raw
	suffix := ""
	if len(compressed) < len(raw) {
		payload = compressed
		suffix = ".z"
	}

	if len(payload) <= TextMaxRaw {
		encoded := string(base85.Encode(payload))
		err := q.nextClient().SendMessage(ctx, chatID, q.prefix+suffix+" "+encoded)
		if err != nil {
			log.Printf("SendQueue: sendMessage error: %v", err)
		}
		return
	}

	if q.enableFiles {
		err := q.nextClient().SendDocument(ctx, chatID, payload, q.prefix+suffix+".bin")
		if err != nil {
			log.Printf("SendQueue: sendDocument error: %v", err)
		}
		return
	}

	// Split into individual text frames
	for _, f := range frames {
		chunkRaw := PackFrames([]Frame{f})
		chunkCompressed := Compress(chunkRaw)
		chunkPayload := chunkRaw
		chunkSuffix := ""
		if len(chunkCompressed) < len(chunkRaw) {
			chunkPayload = chunkCompressed
			chunkSuffix = ".z"
		}
		if len(chunkPayload) <= TextMaxRaw {
			encoded := string(base85.Encode(chunkPayload))
			err := q.nextClient().SendMessage(ctx, chatID, q.prefix+chunkSuffix+" "+encoded)
			if err != nil {
				log.Printf("SendQueue: sendMessage error: %v", err)
			}
		} else {
			log.Printf("SendQueue: frame too large for text and files disabled, dropping %d bytes", len(chunkRaw))
		}
	}
}
