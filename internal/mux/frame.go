package mux

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
)

type Frame struct {
	StreamID string // 32-char hex
	Data     []byte
}

func PackFrames(frames []Frame) []byte {
	size := 0
	for _, f := range frames {
		size += 16 + 4 + len(f.Data)
	}
	buf := make([]byte, 0, size)
	for _, f := range frames {
		id, _ := hex.DecodeString(f.StreamID)
		buf = append(buf, id...)
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(f.Data)))
		buf = append(buf, lenBuf[:]...)
		buf = append(buf, f.Data...)
	}
	return buf
}

func Compress(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func Decompress(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func UnpackFrames(raw []byte) []Frame {
	var frames []Frame
	offset := 0
	for offset+20 <= len(raw) {
		streamID := hex.EncodeToString(raw[offset : offset+16])
		length := binary.BigEndian.Uint32(raw[offset+16 : offset+20])
		offset += 20
		if offset+int(length) > len(raw) {
			log.Printf("UnpackFrames: truncated frame for stream %s (need %d bytes, have %d)", streamID, length, len(raw)-offset)
			break
		}
		data := make([]byte, length)
		copy(data, raw[offset:offset+int(length)])
		offset += int(length)
		frames = append(frames, Frame{StreamID: streamID, Data: data})
	}
	return frames
}
