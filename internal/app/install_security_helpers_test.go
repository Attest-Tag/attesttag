package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"os"
)

func hmacNewSHA256(key []byte) hash.Hash { return hmac.New(sha256.New, key) }
func hexEncode(b []byte) string          { return hex.EncodeToString(b) }
func envMasterKey() string               { return os.Getenv("MASTER_KEY") }
