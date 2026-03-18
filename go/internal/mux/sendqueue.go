package mux

import (
	"context"
	"log"
	"sync"
	"yes/internal/base85"
	"yes/internal/tg"
)

const TextMaxRaw = 3200

type SendQueue struct {
	ch          chan Frame
	client      *tg.Client
	chatID      string
	mu          sync.Mutex
	prefix      string
	enableFiles bool
}

func NewSendQueue(client *tg.Client, chatID, prefix string, enableFiles bool) *SendQueue {
	return &SendQueue{
		ch:          make(chan Frame, 4096),
		client:      client,
		chatID:      chatID,
		prefix:      prefix,
		enableFiles: enableFiles,
	}
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
	if chatID == "" {
		log.Printf("SendQueue: no chat ID set, dropping %d frames", len(frames))
		return
	}

	raw := PackFrames(frames)

	if len(raw) <= TextMaxRaw {
		encoded := string(base85.Encode(raw))
		err := q.client.SendMessage(ctx, chatID, q.prefix+" "+encoded)
		if err != nil {
			log.Printf("SendQueue: sendMessage error: %v", err)
		}
		return
	}

	if q.enableFiles {
		err := q.client.SendDocument(ctx, chatID, raw, q.prefix+".bin")
		if err != nil {
			log.Printf("SendQueue: sendDocument error: %v", err)
		}
		return
	}

	// Split into individual text frames
	for _, f := range frames {
		chunkRaw := PackFrames([]Frame{f})
		if len(chunkRaw) <= TextMaxRaw {
			encoded := string(base85.Encode(chunkRaw))
			err := q.client.SendMessage(ctx, chatID, q.prefix+" "+encoded)
			if err != nil {
				log.Printf("SendQueue: sendMessage error: %v", err)
			}
		} else {
			log.Printf("SendQueue: frame too large for text and files disabled, dropping %d bytes", len(chunkRaw))
		}
	}
}
