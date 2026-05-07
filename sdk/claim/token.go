// Package claim provides claiming-token generation and persistence and the
// poll loop that waits for the Appstore backend to confirm a claim.
package claim

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
)

// alphabet excludes visually ambiguous characters: 0/O, 1/I/L
const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// Generate produces a new random token in the form XXXX-XXXX-XXXX.
// Uses crypto/rand; returns an error if the system CSPRNG is unavailable.
func Generate() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("token: CSPRNG unavailable: %w", err)
	}

	chars := make([]byte, 12)
	for i, v := range b {
		chars[i] = alphabet[int(v)%len(alphabet)]
	}

	return fmt.Sprintf("%s-%s-%s",
		string(chars[0:4]),
		string(chars[4:8]),
		string(chars[8:12]),
	), nil
}

// LoadOrGenerate reads the token from path if it exists and is non-empty,
// otherwise generates a new token and writes it to path.
func LoadOrGenerate(path string, reset bool) (string, error) {
	if !reset {
		if tok, err := load(path); err == nil && tok != "" {
			return tok, nil
		}
	}

	tok, err := Generate()
	if err != nil {
		return "", err
	}
	if err := persist(path, tok); err != nil {
		return tok, fmt.Errorf("token: failed to persist to %s: %w", path, err)
	}
	return tok, nil
}

func load(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func persist(path string, tok string) error {
	return os.WriteFile(path, []byte(tok+"\n"), 0600)
}
