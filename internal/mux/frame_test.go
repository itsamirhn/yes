package mux

import (
	"testing"
)

func TestPackUnpackRoundtrip(t *testing.T) {
	frames := []Frame{
		{StreamID: "0123456789abcdef0123456789abcdef", Data: []byte("hello")},
		{StreamID: "fedcba9876543210fedcba9876543210", Data: []byte("world")},
	}
	packed := PackFrames(frames)
	unpacked := UnpackFrames(packed)

	if len(unpacked) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(unpacked))
	}
	if string(unpacked[0].Data) != "hello" {
		t.Fatalf("frame 0 data = %q, want 'hello'", unpacked[0].Data)
	}
	if string(unpacked[1].Data) != "world" {
		t.Fatalf("frame 1 data = %q, want 'world'", unpacked[1].Data)
	}
}

func TestUnpackFrames_Truncated(t *testing.T) {
	frames := []Frame{
		{StreamID: "0123456789abcdef0123456789abcdef", Data: []byte("hello")},
	}
	packed := PackFrames(frames)

	// Chop off last 2 bytes to simulate truncation
	truncated := packed[:len(packed)-2]
	result := UnpackFrames(truncated)

	// Truncated frame should be dropped (logged, not returned)
	if len(result) != 0 {
		t.Fatalf("expected 0 frames from truncated data, got %d", len(result))
	}
}

func TestCompressDecompressRoundtrip(t *testing.T) {
	data := []byte("some data to compress and decompress")
	compressed := Compress(data)
	decompressed, err := Decompress(compressed)
	if err != nil {
		t.Fatalf("decompress error: %v", err)
	}
	if string(decompressed) != string(data) {
		t.Fatalf("roundtrip failed: got %q", decompressed)
	}
}
