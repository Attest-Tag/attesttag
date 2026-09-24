package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Secrets at rest (connection credentials) are sealed with AES-256-GCM under MASTER_KEY,
// a base64 32-byte key from the environment. Locally a missing key is generated and
// appended to the dotenv file in use (EnvFile) so the developer never has to think about
// it; on Cloud Run it comes from Secret Manager via deploy/gcp/cloudrun.sh.
type Sealer struct {
	aead cipher.AEAD
	// prev opens boxes sealed under MASTER_KEY_PREVIOUS during a rotation: everything is
	// still readable while the new key is the one that seals, and the old key can be dropped
	// once nothing sealed under it remains.
	prev cipher.AEAD
}

// aeadFor builds the cipher for one base64 32-byte key.
func aeadFor(raw string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(key) != 32 {
		return nil, errors.New("MASTER_KEY must be base64 of 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// managedDeployment reports whether this process is running somewhere the key has to come
// from outside: Cloud Run sets K_SERVICE, and REQUIRE_MASTER_KEY=1 forces the same for any
// other host. Generating a key there is never right — nothing persists it, so a fresh one is
// minted on every cold start and every credential sealed under the old one is lost.
func managedDeployment() bool {
	return os.Getenv("K_SERVICE") != "" || os.Getenv("REQUIRE_MASTER_KEY") == "1"
}

func NewSealer() (*Sealer, error) {
	raw := os.Getenv("MASTER_KEY")
	if raw == "" {
		if managedDeployment() {
			return nil, errors.New("MASTER_KEY is not set. Generate one with `openssl rand -base64 32`, put it in .env, and redeploy so it reaches Secret Manager. " +
				"Refusing to generate a throwaway key: it would be replaced on the next restart and every stored credential sealed under it would be lost")
		}
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		raw = base64.StdEncoding.EncodeToString(key)
		file := EnvFile()
		if f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			fmt.Fprintf(f, "\nMASTER_KEY=%s\n", raw)
			f.Close()
			slog.Warn("MASTER_KEY was missing; generated one and appended it to the dotenv file (back it up, it unlocks stored credentials)", "file", file)
		} else {
			return nil, fmt.Errorf("MASTER_KEY is not set and %s could not be written (%w). "+
				"Set MASTER_KEY yourself — a key that is not saved anywhere loses every stored credential on restart", file, err)
		}
		os.Setenv("MASTER_KEY", raw)
	}
	aead, err := aeadFor(raw)
	if err != nil {
		return nil, err
	}
	s := &Sealer{aead: aead}
	if old := os.Getenv("MASTER_KEY_PREVIOUS"); old != "" {
		if s.prev, err = aeadFor(old); err != nil {
			return nil, errors.New("MASTER_KEY_PREVIOUS must be base64 of 32 bytes")
		}
		slog.Info("MASTER_KEY_PREVIOUS is set: boxes sealed under the previous key still open; new seals use the current key")
	}
	return s, nil
}

func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, s.aead.Seal(nil, nonce, plain, nil)...), nil
}

func (s *Sealer) Open(box []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(box) < n {
		return nil, errors.New("ciphertext too short")
	}
	plain, err := s.aead.Open(nil, box[:n], box[n:], nil)
	if err != nil && s.prev != nil {
		return s.prev.Open(nil, box[:n], box[n:], nil)
	}
	return plain, err
}

// ---- purpose-specific keys ----

// masterKeyBytes is the decoded MASTER_KEY, or an error when it is unset or malformed.
func masterKeyBytes() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("MASTER_KEY")))
	if err != nil || len(key) != 32 {
		return nil, errors.New("MASTER_KEY must be base64 of 32 bytes")
	}
	return key, nil
}

var derivedKeys = struct {
	sync.Mutex
	m map[string][]byte
}{m: map[string][]byte{}}

// derivedKey is a key for one purpose, derived from MASTER_KEY with HKDF under a label. A MAC or
// a seal for one use — Configure links, say — then shares nothing but the root with the
// credentials at rest, so revoking or rotating one purpose never has to touch another. It
// returns nil when there is no usable master key; callers must treat nil as "refuse", never as
// an empty key.
func derivedKey(label string) []byte {
	raw := os.Getenv("MASTER_KEY")
	derivedKeys.Lock()
	defer derivedKeys.Unlock()
	if k, ok := derivedKeys.m[raw+"|"+label]; ok {
		return k
	}
	root, err := masterKeyBytes()
	if err != nil {
		return nil
	}
	k, err := hkdf.Key(sha256.New, root, nil, "attest_tag/"+label, 32)
	if err != nil {
		return nil
	}
	derivedKeys.m[raw+"|"+label] = k
	return k
}
