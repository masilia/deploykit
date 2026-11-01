// Package secrets generates random credentials. Generation uses crypto/rand only; nothing here stores or logs.
package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
)

const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// Password returns n random characters from an alphabet without look-alikes (no 0/O, 1/l/I) and without shell or URL
// metacharacters, so it is safe in env files, command lines and URLs. n below 12 is raised to 12.
func Password(n int) (string, error) {
	if n < 12 {
		n = 12
	}
	max := big.NewInt(int64(len(alphabet)))
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("secrets: %w", err)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}

// Hex returns 2*n hexadecimal characters from n random bytes (the `openssl rand -hex n` of ens_gen).
func Hex(n int) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("secrets: Hex needs at least 1 byte")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	return hex.EncodeToString(b), nil
}
