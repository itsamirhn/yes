package transport

import (
	"context"
	"log"
	"regexp"
	"yes/internal/base85"
	"yes/internal/mux"
	"yes/internal/tg"
)

// Decoder decodes KCP packets from Telegram messages for a specific receive prefix.
type Decoder struct {
	reData       *regexp.Regexp
	reCompressed *regexp.Regexp
	binName      string
	binZName     string
}

// NewDecoder creates a decoder that matches the given recvPrefix.
// Client receives "S" (server→client), server receives "C" (client→server).
func NewDecoder(recvPrefix string) *Decoder {
	return &Decoder{
		reData:       regexp.MustCompile(`^` + regexp.QuoteMeta(recvPrefix) + ` (\S+)$`),
		reCompressed: regexp.MustCompile(`^` + regexp.QuoteMeta(recvPrefix) + `\.z (\S+)$`),
		binName:      recvPrefix + ".bin",
		binZName:     recvPrefix + ".z.bin",
	}
}

// DecodeKCPPacket attempts to decode a KCP packet from a Telegram message.
// Returns the raw KCP segment bytes, or nil if not a matching packet.
func (d *Decoder) DecodeKCPPacket(ctx context.Context, msg *tg.Message, pollClient *tg.Client, enableFiles bool) []byte {
	if msg.Text != "" {
		if m := d.reCompressed.FindStringSubmatch(msg.Text); m != nil {
			compressed, err := base85.Decode([]byte(m[1]))
			if err != nil {
				log.Printf("KCP base85 decode error: %v", err)
				return nil
			}
			raw, err := mux.Decompress(compressed)
			if err != nil {
				log.Printf("KCP decompress error: %v", err)
				return nil
			}
			return raw
		}
		if m := d.reData.FindStringSubmatch(msg.Text); m != nil {
			raw, err := base85.Decode([]byte(m[1]))
			if err != nil {
				log.Printf("KCP base85 decode error: %v", err)
				return nil
			}
			return raw
		}
	}

	if enableFiles && msg.Document != nil {
		fn := msg.Document.FileName
		if fn == d.binName || fn == d.binZName {
			raw, err := pollClient.DownloadDocument(ctx, msg.Document.FileID)
			if err != nil {
				log.Printf("KCP download document error: %v", err)
				return nil
			}
			if fn == d.binZName {
				raw, err = mux.Decompress(raw)
				if err != nil {
					log.Printf("KCP decompress error: %v", err)
					return nil
				}
			}
			return raw
		}
	}

	return nil
}
