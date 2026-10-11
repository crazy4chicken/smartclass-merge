package domain

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// alphabet is Crockford base32: no I, L, O or U.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a new ULID for the current time: 48 bits of millisecond
// timestamp followed by 80 bits from crypto/rand, rendered as 26 Crockford
// base32 characters. The encoding mirrors the dispatchub id.New helper, so
// every service in the fleet mints text ids the same way, and it needs no
// dependency.
func NewID() string {
	var raw [16]byte
	ms := uint64(time.Now().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	// crypto/rand.Read never returns an error: it panics on unrecoverable failure.
	_, _ = rand.Read(raw[6:])
	return encode(raw)
}

// encode renders 16 bytes as a 26 character Crockford base32 ULID.
func encode(raw [16]byte) string {
	hi := binary.BigEndian.Uint64(raw[0:8])
	lo := binary.BigEndian.Uint64(raw[8:16])
	bit := func(pos int) uint64 {
		switch {
		case pos < 0 || pos >= 128:
			return 0
		case pos < 64:
			return lo >> uint(pos) & 1
		default:
			return hi >> uint(pos-64) & 1
		}
	}
	var out [26]byte
	// 26 characters carry 130 bits, so the two leading bits are zero padding.
	for i := range out {
		top := 129 - 5*i
		var char uint64
		for k := range 5 {
			char = char<<1 | bit(top-k)
		}
		out[i] = alphabet[char]
	}
	return string(out[:])
}
