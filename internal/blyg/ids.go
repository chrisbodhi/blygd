// Package blyg is the protocol layer: wire types, identity, hashing, and the
// pure renderers for every file on the public surface. Nothing here touches
// the database or the network.
//
// Section numbers (§) refer to blygger-spec docs/protocol-v0.4.md.
package blyg

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// ProtocolVersion is the manifest/item "blyg" key (§3.1).
const ProtocolVersion = "0.4"

// IDAlphabet is lowercase Crockford base32 without i, l, o, u (§5.1).
const IDAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// IDLen is the length of an item id in characters.
const IDLen = 26

// NewID returns 128 cryptographically random bits as 26 characters of
// lowercase Crockford base32 (§5.1).
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for an id generator.
		panic("blyg: crypto/rand failed: " + err.Error())
	}
	return EncodeID(b)
}

// EncodeID encodes 16 bytes big-endian into 26 base32 characters. 26×5 = 130
// bits, so the two most significant bits are zero padding — the encoding the
// reference client uses, which means the first character is always 0–7.
func EncodeID(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [IDLen]byte
	for i := IDLen - 1; i >= 0; i-- {
		out[i] = IDAlphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ValidID reports whether s has the shape of an item id: 26 characters from
// the alphabet. It is deliberately lenient about the padding bits, because
// readers must accept other publishers' ids as they find them.
func ValidID(s string) bool {
	if len(s) != IDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIDChar(s[i]) {
			return false
		}
	}
	return true
}

func isIDChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return c != 'i' && c != 'l' && c != 'o' && c != 'u'
	}
	return false
}

// ContentHash is "sha256:" + hex(SHA-256(content_md as UTF-8)) (§5.1). It
// covers content_md only. A withdrawal endcap hashes the empty string.
func ContentHash(contentMD string) string {
	sum := sha256.Sum256([]byte(contentMD))
	return "sha256:" + hex.EncodeToString(sum[:])
}
