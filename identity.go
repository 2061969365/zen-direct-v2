package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
	"time"
)

var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const sessionTimestampWindow = 30 * 24 * time.Hour

// bootNonce separates the sessions one gateway process derives from those its
// earlier runs derived. It is minted once per process and mixed into every
// synthesized session, so restarting the daemon re-mints the upstream session
// ids for the same conversations, while a single run keeps one stable id per
// conversation. Official client sessions bypass it and stay verbatim.
var bootNonce = mintBootNonce()

func mintBootNonce() []byte {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return buf
}

// CanonicalSessionID returns signal unchanged when it already carries an
// official OpenCode session minted by the client's Identifier.descending()
// (preserving upstream prompt-cache affinity). Any other downstream identity
// is re-encoded into that exact layout — inverted 48-bit millisecond
// timestamp with counter 1, so the low 12 hex digits are ffe — keeping one
// stable session per conversation for the life of the process.
func CanonicalSessionID(signal string) string {
	if isOfficialSessionID(signal) {
		return signal
	}
	hasher := sha256.New()
	hasher.Write([]byte("ses\x00"))
	hasher.Write(bootNonce)
	hasher.Write([]byte{0})
	hasher.Write([]byte(signal))
	sum := hasher.Sum(nil)
	timePart := descendingTimePart(time.Now(), sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// isOfficialSessionID reports whether signal is a session minted by the
// official client's Identifier.descending().
func isOfficialSessionID(signal string) bool {
	return canonicalSessionPattern.MatchString(signal) && len(signal) >= 16 && descendingTimePartPattern.MatchString(signal[4:16])
}

func descendingTimePart(now time.Time, hash []byte) string {
	window := uint64(sessionTimestampWindow / time.Millisecond)
	offset := (uint64(hash[0])<<24 | uint64(hash[1])<<16 | uint64(hash[2])<<8 | uint64(hash[3])) % window
	base := now.UTC().Truncate(24 * time.Hour).Add(-time.Duration(offset) * time.Millisecond)
	current := uint64(base.UnixMilli())<<12 | 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}
