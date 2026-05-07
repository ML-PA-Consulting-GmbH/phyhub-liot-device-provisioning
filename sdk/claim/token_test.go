package claim

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tokenFormat matches the documented XXXX-XXXX-XXXX shape using only
// characters from the unambiguous alphabet (no 0/O, no 1/I/L).
var tokenFormat = regexp.MustCompile(`^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{4}-[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{4}-[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{4}$`)

func TestGenerate_Format(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tokenFormat.MatchString(tok) {
		t.Errorf("token %q does not match XXXX-XXXX-XXXX (alphabet excludes 0/O/1/I/L)", tok)
	}
}

func TestGenerate_NoAmbiguousChars(t *testing.T) {
	// Pull a sample of tokens; collectively they must not contain any
	// visually-ambiguous character. This guards against someone widening
	// the alphabet without thinking about what the operator has to type.
	const samples = 200
	bad := []rune{'0', 'O', '1', 'I', 'L'}
	for i := 0; i < samples; i++ {
		tok, err := Generate()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, b := range bad {
			if strings.ContainsRune(tok, b) {
				t.Fatalf("token %q contains ambiguous char %q", tok, b)
			}
		}
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	// 12 chars from a 31-char alphabet is ~59 bits of entropy; collisions
	// in 1k draws would mean the CSPRNG is broken or someone replaced
	// rand.Read with math/rand.
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		tok, err := Generate()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token %q after %d draws (entropy regression?)", tok, i)
		}
		seen[tok] = struct{}{}
	}
}

func TestLoadOrGenerate_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	tok1, err := LoadOrGenerate(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tokenFormat.MatchString(tok1) {
		t.Errorf("generated token has wrong shape: %q", tok1)
	}

	// Second call without reset must return the persisted token verbatim.
	tok2, err := LoadOrGenerate(path, false)
	if err != nil {
		t.Fatalf("unexpected error on reload: %v", err)
	}
	if tok2 != tok1 {
		t.Errorf("token not persisted: first=%q second=%q", tok1, tok2)
	}
}

func TestLoadOrGenerate_FileMode0600(t *testing.T) {
	// The token is a credential; the file mode matters. If someone widens
	// it to 0644 a regression test should scream: token files end up in
	// /var/lib/snapd which other unprivileged readers can traverse.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	if _, err := LoadOrGenerate(path, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("file mode: got %#o, want 0600", mode)
	}
}

func TestLoadOrGenerate_ResetForcesNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	tok1, err := LoadOrGenerate(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tok2, err := LoadOrGenerate(path, true)
	if err != nil {
		t.Fatalf("unexpected error on reset: %v", err)
	}
	if tok2 == tok1 {
		t.Errorf("reset=true returned the same token %q (should have rotated)", tok1)
	}

	// And the new token is what's now on disk.
	tok3, err := LoadOrGenerate(path, false)
	if err != nil {
		t.Fatalf("unexpected error on third load: %v", err)
	}
	if tok3 != tok2 {
		t.Errorf("reset did not persist: on-disk=%q, expected=%q", tok3, tok2)
	}
}

func TestLoadOrGenerate_EmptyFileRegenerates(t *testing.T) {
	// An empty (or whitespace-only) token file is treated as missing and
	// regenerated. Otherwise a corrupt file would brick the device into
	// looping on a zero-length token forever.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("   \n"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tok, err := LoadOrGenerate(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tokenFormat.MatchString(tok) {
		t.Errorf("expected freshly-generated token, got %q", tok)
	}
}

func TestLoadOrGenerate_PersistFailureReturnsToken(t *testing.T) {
	// If we can't write to the configured path (read-only fs, missing
	// dir, etc.), the operator still needs the token displayed, so the
	// function returns it together with the error rather than swallowing
	// the token.
	path := filepath.Join(t.TempDir(), "no-such-dir", "token")

	tok, err := LoadOrGenerate(path, false)
	if err == nil {
		t.Fatal("expected persist error, got nil")
	}
	if !tokenFormat.MatchString(tok) {
		t.Errorf("token not returned alongside error: %q", tok)
	}
}
