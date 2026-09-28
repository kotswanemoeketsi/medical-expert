package secure

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238) over HMAC-SHA1, six digits, thirty-second steps: what every
// authenticator app implements. SHA-1 is correct here — it is an HMAC keyed
// with a shared secret, not a collision-resistant digest.

const (
	totpDigits    = 6
	totpPeriod    = 30
	totpSecretLen = 20 // 160 bits, the RFC 4226 recommendation
	totpSkewSteps = 1  // accept one step either side for clock drift
)

var (
	ErrInvalidCode  = errors.New("secure: invalid code")
	ErrCodeReplayed = errors.New("secure: code already used")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh base32 secret, unpadded, as apps expect it.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("secure: totp secret: %w", err)
	}
	return b32.EncodeToString(raw), nil
}

// TOTPProvisioningURI builds the otpauth:// URI an authenticator app scans.
// It contains the secret, so it must only ever be rendered on the client.
func TOTPProvisioningURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func decodeSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	key, err := b32.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, errors.New("secure: malformed totp secret")
	}
	return key, nil
}

// CounterAt is the RFC 6238 time step containing t.
func CounterAt(t time.Time) uint64 { return uint64(t.Unix()) / totpPeriod }

// TOTPCode computes the code for one counter value (RFC 4226 HOTP).
func TOTPCode(secret string, counter uint64) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, counter), nil
}

func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	// Dynamic truncation, RFC 4226 section 5.3.
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	return fmt.Sprintf("%0*d", totpDigits, bin%1_000_000)
}

// VerifyTOTP checks code against the steps around at. It returns the counter
// that matched so the caller can store it: lastUsed is the counter of the
// last accepted code, and anything at or before it is refused as a replay.
// A spent code stays spent even inside its thirty seconds.
//
// The caller must also persist the returned counter with a conditional
// update (WHERE mfa_last_counter < ?), because two requests can pass this
// check concurrently; the database decides which one wins.
func VerifyTOTP(secret, code string, at time.Time, lastUsed *uint64) (uint64, error) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, ErrInvalidCode
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, ErrInvalidCode
		}
	}
	key, err := decodeSecret(secret)
	if err != nil {
		return 0, err
	}
	now := CounterAt(at)
	replayed := false
	for delta := -totpSkewSteps; delta <= totpSkewSteps; delta++ {
		if delta < 0 && now < uint64(-delta) {
			continue
		}
		counter := uint64(int64(now) + int64(delta))
		if subtle.ConstantTimeCompare([]byte(hotp(key, counter)), []byte(code)) != 1 {
			continue
		}
		if lastUsed != nil && counter <= *lastUsed {
			replayed = true
			continue
		}
		return counter, nil
	}
	if replayed {
		return 0, ErrCodeReplayed
	}
	return 0, ErrInvalidCode
}

// ---------------------------------------------------------------------------
// Recovery codes
// ---------------------------------------------------------------------------

// RecoveryCodeCount codes are issued at a time, each usable once.
const RecoveryCodeCount = 10

// recoveryAlphabet omits I, O, 0 and 1, which get misread off a printout.
const recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewRecoveryCodes returns RecoveryCodeCount codes of 16 characters from a
// 32-symbol alphabet: 80 bits each, formatted XXXX-XXXX-XXXX-XXXX.
func NewRecoveryCodes() ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("secure: recovery code: %w", err)
		}
		var b strings.Builder
		for j, r := range raw {
			if j > 0 && j%4 == 0 {
				b.WriteByte('-')
			}
			// 256 is a multiple of 32, so the modulo introduces no bias.
			b.WriteByte(recoveryAlphabet[int(r)%len(recoveryAlphabet)])
		}
		codes[i] = b.String()
	}
	return codes, nil
}

// HashRecoveryCode normalises what a person typed (case, spaces, dashes) and
// digests it. SHA-256 rather than a password hash: each code carries 80 bits
// of randomness, so there is nothing to brute-force, and hashing ten codes at
// 600k iterations per sign-in would be a denial of service on ourselves.
func HashRecoveryCode(code string) [32]byte {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return sha256.Sum256([]byte(b.String()))
}
