package msg

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"sync"
	"time"
)

var (
	uuidMu   sync.Mutex
	uuidLast int64
	uuidSeq  uint16
	nowMilli = func() int64 { return time.Now().UnixMilli() }
	uuidV7RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// NewUUIDv7 returns an RFC 9562 version 7 UUID. Within one millisecond the
// 12-bit rand_a field is a counter seeded randomly, so ids from one process
// sort in generation order (RFC 9562 §6.2 method 1).
func NewUUIDv7() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	uuidMu.Lock()
	ms := nowMilli()
	if ms <= uuidLast {
		ms = uuidLast
		uuidSeq++
		if uuidSeq > 0x0fff {
			ms++
			uuidSeq = binary.BigEndian.Uint16(b[6:8]) & 0x07ff
		}
	} else {
		uuidSeq = binary.BigEndian.Uint16(b[6:8]) & 0x07ff
	}
	uuidLast = ms
	seq := uuidSeq
	uuidMu.Unlock()

	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(seq>>8)&0x0f
	b[7] = byte(seq)
	b[8] = b[8]&0x3f | 0x80

	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

func IsUUIDv7(s string) bool { return uuidV7RE.MatchString(s) }

// UUIDv7Time returns the millisecond timestamp encoded in a version 7 UUID.
func UUIDv7Time(s string) (time.Time, bool) {
	if !IsUUIDv7(s) {
		return time.Time{}, false
	}
	b, err := hex.DecodeString(s[0:8] + s[9:13])
	if err != nil {
		return time.Time{}, false
	}
	var ms int64
	for _, c := range b {
		ms = ms<<8 | int64(c)
	}
	return time.UnixMilli(ms), true
}
