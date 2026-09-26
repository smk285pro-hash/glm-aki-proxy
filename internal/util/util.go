// Package util holds tiny shared helpers (uuid, encodings).
package util

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// UUIDv4 returns a random v4 UUID string.
func UUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// B64Std base64-encodes with the standard alphabet (with padding).
func B64Std(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// ShortToken keeps first 8 chars for logs (never dump full secrets).
func ShortToken(s string) string {
	if len(s) > 8 {
		return s[:8] + "..."
	}
	return s
}

// EstimateTokens is a rough char/4 heuristic for usage reporting.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}
