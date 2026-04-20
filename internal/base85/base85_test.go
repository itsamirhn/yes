package base85

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x01},
		{0x01, 0x02},
		{0x01, 0x02, 0x03},
		{0x01, 0x02, 0x03, 0x04},
		{0x01, 0x02, 0x03, 0x04, 0x05},
		[]byte("hello world this is a test of base85 encoding"),
	}
	for _, tc := range cases {
		encoded := Encode(tc)
		decoded, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode error for input len %d: %v", len(tc), err)
		}
		if !bytes.Equal(decoded, tc) {
			t.Fatalf("roundtrip failed for len %d: got %x, want %x", len(tc), decoded, tc)
		}
	}
}

func TestDecode_InvalidTail(t *testing.T) {
	// 6 chars = 1 full group (5) + tail of 1 = invalid
	_, err := Decode([]byte("012345A"))
	// len=7, 7%5=2 which is valid tail
	// We need exactly tail=1: len%5==1, so len=6
	_, err = Decode([]byte("012345"))
	if err == nil {
		t.Fatal("expected error for tail=1, got nil")
	}
}

func TestDecode_InvalidCharacter(t *testing.T) {
	_, err := Decode([]byte("hello\x00"))
	if err == nil {
		t.Fatal("expected error for invalid character")
	}
}
