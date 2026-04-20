package mux

import (
	"io"
	"testing"
)

func TestStreamBuffer_DrainBeforeEOF(t *testing.T) {
	buf := NewStreamBuffer()
	buf.Write([]byte("hello"))
	buf.Write([]byte("world"))
	buf.Close()

	// First read should get data, not EOF
	data, err := buf.Read(4096)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "helloworld" {
		t.Fatalf("expected 'helloworld', got %q", string(data))
	}

	// Next read should return EOF
	_, err = buf.Read(4096)
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestStreamBuffer_WriteAfterClose(t *testing.T) {
	buf := NewStreamBuffer()
	buf.Close()

	// Should not panic
	buf.Write([]byte("data"))
}

func TestStreamBuffer_CloseIdempotent(t *testing.T) {
	buf := NewStreamBuffer()
	buf.Close()
	buf.Close() // should not panic
}
