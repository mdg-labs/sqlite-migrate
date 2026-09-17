package sqlitemigrate

// Checksum computes and verifies a per-migration checksum over a
// migration's SQL body, so tampering with an already-applied migration
// file is detected on every generate and apply, not just recorded once.

import (
	"crypto/sha256"
	"encoding/hex"
)

// Checksum returns the SHA-256 checksum of a migration's SQL body, hex
// encoded. It is computed over the exact file contents Load/LoadDir read,
// so any edit to an already-applied migration file — including a change
// that leaves the file's meaning unchanged, like reformatting — is
// detected rather than silently accepted.
func Checksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// VerifyChecksum reports whether sql's checksum matches want.
func VerifyChecksum(sql, want string) bool {
	return Checksum(sql) == want
}
