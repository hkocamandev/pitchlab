// Package auth holds password hashing and token issuance.
//
// Both are small, self-contained and security-critical, which is exactly the
// combination that argues for keeping them in one reviewable place rather than
// scattered through the handlers that use them.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters.
//
// Argon2id rather than bcrypt: it is the Password Hashing Competition winner
// and the OWASP first choice for new applications. Its advantage over bcrypt
// is memory-hardness -- bcrypt's cost is CPU only, so a GPU or ASIC attacks it
// far more cheaply than it attacks a function that also demands 64 MB per
// guess.
//
// The values follow the OWASP Password Storage Cheat Sheet's second
// recommended configuration (64 MiB, 3 iterations, 4 lanes). Measured on this
// machine at roughly 50 ms per hash, which is the intended shape: slow enough
// that offline guessing is expensive, fast enough that a login is not.
const (
	argonMemory      uint32 = 64 * 1024 // KiB
	argonIterations  uint32 = 3
	argonParallelism uint8  = 4
	argonSaltLength  int    = 16
	argonKeyLength   uint32 = 32
)

// ErrInvalidHash reports a stored value this code cannot interpret.
var ErrInvalidHash = errors.New("password hash is not a recognised Argon2id string")

// HashPassword returns a PHC-format Argon2id string.
//
// The parameters travel inside the string rather than living in a constant the
// verifier reads. That is what makes the cost factor upgradable: an old hash
// keeps verifying with the parameters it was created with, while new hashes
// use the current ones. A verifier that assumed today's constants would reject
// every password the moment the cost was raised.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt,
		argonIterations, argonMemory, argonParallelism, argonKeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether the password matches the encoded hash.
//
// Returns only a boolean and a parse error, never "wrong password" as an
// error: a caller that can distinguish "no such user" from "wrong password"
// eventually leaks that distinction to whoever is guessing.
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt,
		params.iterations, params.memory, params.parallelism, uint32(len(want)))

	// Constant-time comparison. A byte-by-byte compare returns faster on an
	// early mismatch, and that timing difference is measurable across a
	// network often enough to recover a hash one byte at a time.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		// A hash written by a different Argon2 version cannot be verified by
		// this one. Failing loudly beats silently rejecting every login.
		return p, nil, nil, fmt.Errorf(
			"%w: version %d, this build understands %d",
			ErrInvalidHash, version, argon2.Version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.memory, &p.iterations, &p.parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	return p, salt, key, nil
}

// DummyVerify burns the same work as a real verification.
//
// Called when the username does not exist. Without it, a missing user returns
// in microseconds and a real one in fifty milliseconds, and that difference
// enumerates accounts over the network -- which is the whole point of not
// distinguishing the two failures in the response body.
func DummyVerify(password string) {
	_ = argon2.IDKey([]byte(password), make([]byte, argonSaltLength),
		argonIterations, argonMemory, argonParallelism, argonKeyLength)
}
