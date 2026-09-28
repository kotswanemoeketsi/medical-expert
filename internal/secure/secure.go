// Package secure holds the system's cryptographic primitives: password
// hashing, session tokens and (in totp.go) the second factor.
//
// Everything here is either standard-library crypto or a short, well-known
// construction verified against published test vectors. Nothing is invented.
package secure

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hasher hashes and verifies passwords. An interface so the algorithm can be
// swapped (argon2id is the documented next step) without touching callers:
// the encoded form names its own algorithm, so old hashes keep verifying.
type Hasher interface {
	Hash(password string) (string, error)
	// Verify reports whether password matches encoded, and whether the hash
	// should be replaced because it was made with weaker parameters.
	Verify(encoded, password string) (ok bool, needsRehash bool, err error)
}

// DefaultPBKDF2Iterations follows the OWASP recommendation for
// PBKDF2-HMAC-SHA256.
const DefaultPBKDF2Iterations = 600_000

const (
	pbkdf2Prefix  = "pbkdf2_sha256"
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32
)

// ErrMalformedHash means a stored hash could not be parsed. It is never a
// user error; it means the database holds something unexpected.
var ErrMalformedHash = errors.New("secure: malformed password hash")

// PBKDF2Hasher implements Hasher with PBKDF2-HMAC-SHA256.
//
// Encoded form: pbkdf2_sha256$<iterations>$<salt b64>$<hash b64>
type PBKDF2Hasher struct {
	Iterations int
}

func NewPBKDF2Hasher() *PBKDF2Hasher { return &PBKDF2Hasher{Iterations: DefaultPBKDF2Iterations} }

func (h *PBKDF2Hasher) iterations() int {
	if h.Iterations <= 0 {
		return DefaultPBKDF2Iterations
	}
	return h.Iterations
}

func (h *PBKDF2Hasher) Hash(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secure: salt: %w", err)
	}
	iter := h.iterations()
	key := PBKDF2SHA256([]byte(password), salt, iter, pbkdf2KeyLen)
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func (h *PBKDF2Hasher) Verify(encoded, password string) (bool, bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Prefix {
		return false, false, ErrMalformedHash
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false, false, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false, false, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, false, ErrMalformedHash
	}
	got := PBKDF2SHA256([]byte(password), salt, iter, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, iter < h.iterations(), nil
}

// PBKDF2SHA256 is RFC 8018 PBKDF2 with HMAC-SHA256.
func PBKDF2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var blockIndex [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		blockIndex[0] = byte(block >> 24)
		blockIndex[1] = byte(block >> 16)
		blockIndex[2] = byte(block >> 8)
		blockIndex[3] = byte(block)
		prf.Write(blockIndex[:])
		dk = prf.Sum(dk)
		t := dk[len(dk)-hashLen:]
		copy(u, t)

		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = u[:0]
			u = prf.Sum(u)
			for x := range u {
				t[x] ^= u[x]
			}
		}
	}
	return dk[:keyLen]
}

// ---------------------------------------------------------------------------
// Password policy (BR-1.2)
// ---------------------------------------------------------------------------

const (
	MinPasswordLength = 12
	MaxPasswordLength = 256
)

// weakFragments are rejected anywhere in a password, case-insensitively.
// Length alone does not stop "Password1234", which is the first thing an
// attacker tries against a medical practice.
var weakFragments = []string{
	"password", "passw0rd", "123456", "654321", "qwerty", "letmein", "welcome",
	"admin", "clinic", "medical", "doctor", "iloveyou", "abc123", "111111",
}

// PasswordPolicyError explains why a password was refused, in words a user
// can act on.
type PasswordPolicyError struct{ Reason string }

func (e *PasswordPolicyError) Error() string { return e.Reason }

// CheckPasswordPolicy returns nil or a *PasswordPolicyError. The account's
// email and name are passed so a password cannot simply repeat them.
func CheckPasswordPolicy(password string, personal ...string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength {
		return &PasswordPolicyError{fmt.Sprintf("must be at least %d characters", MinPasswordLength)}
	}
	if n > MaxPasswordLength {
		return &PasswordPolicyError{fmt.Sprintf("must be at most %d characters", MaxPasswordLength)}
	}
	lower := strings.ToLower(password)
	for _, f := range weakFragments {
		if strings.Contains(lower, f) {
			return &PasswordPolicyError{"contains a common word or sequence that attackers try first"}
		}
	}
	distinct := map[rune]bool{}
	for _, r := range password {
		distinct[r] = true
	}
	if len(distinct) < 5 {
		return &PasswordPolicyError{"uses too few different characters"}
	}
	for _, p := range personal {
		p = strings.ToLower(p)
		if at := strings.IndexByte(p, '@'); at > 0 {
			p = p[:at]
		}
		words := strings.FieldsFunc(p, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		for _, w := range words {
			if utf8.RuneCountInString(w) >= 4 && strings.Contains(lower, w) {
				return &PasswordPolicyError{"must not contain your name or email address"}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Session tokens
// ---------------------------------------------------------------------------

// NewSessionToken returns a 256-bit random bearer token and the SHA-256
// digest that is stored in its place. The plaintext is never persisted.
func NewSessionToken() (token string, digest [32]byte, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", digest, fmt.Errorf("secure: token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken digests a bearer token for lookup. SHA-256 rather than a slow
// hash: the input is 256 bits of randomness, so there is nothing to guess,
// and this runs on every request.
func HashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// RandomPassword returns a password that satisfies the policy, for bootstrap
// accounts that must change it on first use.
func RandomPassword() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	for {
		b := make([]byte, 20)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		pw := string(b[:5]) + "-" + string(b[5:10]) + "-" + string(b[10:15]) + "-" + string(b[15:])
		if CheckPasswordPolicy(pw) == nil {
			return pw, nil
		}
	}
}
