package mux

import (
	"io"
	"sync"
)

type StreamBuffer struct {
	ch     chan []byte
	buf    []byte
	closed bool
	once   sync.Once
}

func NewStreamBuffer() *StreamBuffer {
	return &StreamBuffer{
		ch: make(chan []byte, 256),
	}
}

func (s *StreamBuffer) Write(data []byte) {
	defer func() { recover() }() // ignore send on closed channel
	s.ch <- data
}

func (s *StreamBuffer) Read(size int) ([]byte, error) {
	for len(s.buf) == 0 {
		data, ok := <-s.ch
		if !ok {
			return nil, io.EOF
		}
		s.buf = append(s.buf, data...)
	}

	// Drain any additional available data
	closed := false
	for len(s.buf) < size {
		select {
		case data, ok := <-s.ch:
			if !ok {
				closed = true
				goto done
			}
			s.buf = append(s.buf, data...)
		default:
			goto done
		}
	}
done:

	n := size
	if n > len(s.buf) {
		n = len(s.buf)
	}
	result := make([]byte, n)
	copy(result, s.buf[:n])
	s.buf = s.buf[n:]

	// Return EOF only after all buffered data has been consumed
	if closed && len(s.buf) == 0 {
		return result, io.EOF
	}
	return result, nil
}

func (s *StreamBuffer) Close() {
	s.once.Do(func() { close(s.ch) })
}
