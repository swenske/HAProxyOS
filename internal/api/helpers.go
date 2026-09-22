package api

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
