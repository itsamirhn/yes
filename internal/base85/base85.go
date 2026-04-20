package base85

import (
	"encoding/binary"
	"errors"
)

// RFC 1924 base85 alphabet (compatible with Python's base64.b85encode/b85decode)
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

var (
	encode [85]byte
	decode [256]byte
)

func init() {
	copy(encode[:], alphabet)
	for i := range decode {
		decode[i] = 0xff
	}
	for i, c := range alphabet {
		decode[c] = byte(i)
	}
}

func Encode(src []byte) []byte {
	n := len(src)
	fullGroups := n / 4
	tail := n % 4
	outLen := fullGroups * 5
	if tail > 0 {
		outLen += tail + 1
	}
	dst := make([]byte, outLen)

	si, di := 0, 0
	for i := 0; i < fullGroups; i++ {
		v := binary.BigEndian.Uint32(src[si:])
		si += 4
		for j := 4; j >= 0; j-- {
			dst[di+j] = encode[v%85]
			v /= 85
		}
		di += 5
	}

	if tail > 0 {
		// Pad with zeros to 4 bytes
		var padded [4]byte
		copy(padded[:], src[si:])
		v := binary.BigEndian.Uint32(padded[:])
		var tmp [5]byte
		for j := 4; j >= 0; j-- {
			tmp[j] = encode[v%85]
			v /= 85
		}
		copy(dst[di:], tmp[:tail+1])
	}

	return dst
}

func Decode(src []byte) ([]byte, error) {
	n := len(src)
	fullGroups := n / 5
	tail := n % 5
	outLen := fullGroups * 4
	if tail > 0 {
		outLen += tail - 1
	}
	dst := make([]byte, outLen)

	si, di := 0, 0
	for i := 0; i < fullGroups; i++ {
		var v uint64
		for j := 0; j < 5; j++ {
			d := decode[src[si+j]]
			if d == 0xff {
				return nil, errors.New("base85: invalid character")
			}
			v = v*85 + uint64(d)
		}
		if v > 0xFFFFFFFF {
			return nil, errors.New("base85: value overflow")
		}
		binary.BigEndian.PutUint32(dst[di:], uint32(v))
		si += 5
		di += 4
	}

	if tail == 1 {
		return nil, errors.New("base85: invalid input length")
	}
	if tail > 1 {
		// Pad with ~ (value 84)
		var padded [5]byte
		copy(padded[:], src[si:])
		for j := tail; j < 5; j++ {
			padded[j] = '~'
		}
		var v uint64
		for j := 0; j < 5; j++ {
			d := decode[padded[j]]
			if d == 0xff {
				return nil, errors.New("base85: invalid character")
			}
			v = v*85 + uint64(d)
		}
		if v > 0xFFFFFFFF {
			return nil, errors.New("base85: value overflow")
		}
		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], uint32(v))
		copy(dst[di:], tmp[:tail-1])
	}

	return dst, nil
}
