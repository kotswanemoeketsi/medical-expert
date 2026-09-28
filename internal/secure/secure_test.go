package secure

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Known-answer tests: PBKDF2-HMAC-SHA256, password "password", salt "salt",
// 32-byte output — the widely published vectors, cross-checked against
// Python's hashlib.pbkdf2_hmac.
func TestPBKDF2KnownAnswer(t *testing.T) {
	cases := []struct {
		iter int
		want string
	}{
		{1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(PBKDF2SHA256([]byte("password"), []byte("salt"), c.iter, 32))
		if got != c.want {
			t.Errorf("iter=%d: got %s, want %s", c.iter, got, c.want)
		}
	}
}

func TestPBKDF2HasherRoundTrip(t *testing.T) {
	h := &PBKDF2Hasher{Iterations: 1000}
	enc, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "pbkdf2_sha256$1000$") {
		t.Fatalf("unexpected encoding %q", enc)
	}
	ok, rehash, err := h.Verify(enc, "correct horse battery staple")
	if err != nil || !ok || rehash {
		t.Fatalf("verify: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	ok, _, err = h.Verify(enc, "correct horse battery stapler")
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
}

func TestPBKDF2SaltsDiffer(t *testing.T) {
	h := &PBKDF2Hasher{Iterations: 1000}
	a, _ := h.Hash("same password here")
	b, _ := h.Hash("same password here")
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt is not random")
	}
}

func TestPBKDF2FlagsWeakerHashesForRehash(t *testing.T) {
	old := &PBKDF2Hasher{Iterations: 1000}
	enc, _ := old.Hash("a perfectly fine passphrase")
	current := &PBKDF2Hasher{Iterations: 2000}
	ok, rehash, err := current.Verify(enc, "a perfectly fine passphrase")
	if err != nil || !ok || !rehash {
		t.Fatalf("expected ok and needsRehash, got ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestPBKDF2RejectsMalformedHashes(t *testing.T) {
	h := NewPBKDF2Hasher()
	for _, enc := range []string{"", "plain", "bcrypt$10$x$y", "pbkdf2_sha256$x$c2FsdA$aGFzaA", "pbkdf2_sha256$10$$aGFzaA"} {
		if _, _, err := h.Verify(enc, "whatever"); !errors.Is(err, ErrMalformedHash) {
			t.Errorf("Verify(%q) err = %v, want ErrMalformedHash", enc, err)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	good := []string{"blue-kettle-orbit-73", "Umzimkulu river 1998!", "tjhoko ya lefatshe"}
	for _, p := range good {
		if err := CheckPasswordPolicy(p, "thandi@greenside.co.za", "Thandi Mokoena"); err != nil {
			t.Errorf("rejected acceptable password %q: %v", p, err)
		}
	}
	bad := []string{
		"short1!",               // too short
		"Password2026!",         // weak fragment
		"GreensideClinic99",     // weak fragment "clinic"
		"aaaaaaaaaaaaaaaa",      // too few distinct characters
		"thandi-loves-hiking",   // contains the email local part
		"mokoena-rides-bikes",   // contains a word of the email local part
		strings.Repeat("x", 12), // too few distinct characters
		strings.Repeat("ab3$", 65),
	}
	for _, p := range bad {
		if err := CheckPasswordPolicy(p, "thandi.mokoena@greenside.co.za"); err == nil {
			t.Errorf("accepted weak password %q", p)
		}
	}
}

func TestRandomPasswordMeetsPolicy(t *testing.T) {
	for i := 0; i < 20; i++ {
		pw, err := RandomPassword()
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckPasswordPolicy(pw); err != nil {
			t.Fatalf("generated password %q fails policy: %v", pw, err)
		}
	}
}

func TestSessionTokens(t *testing.T) {
	tok, digest, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 {
		t.Fatalf("expected 43 base64url characters for 256 bits, got %d", len(tok))
	}
	if HashToken(tok) != digest {
		t.Fatal("HashToken does not reproduce the stored digest")
	}
	tok2, _, _ := NewSessionToken()
	if tok == tok2 {
		t.Fatal("two tokens are identical")
	}
}

// ---------------------------------------------------------------------------
// TOTP
// ---------------------------------------------------------------------------

// rfcSecret is base32("12345678901234567890"), the RFC 6238 SHA-1 test key.
var rfcSecret = b32.EncodeToString([]byte("12345678901234567890"))

func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	// RFC 6238 Appendix B gives eight digits; six-digit codes are the last six.
	vectors := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, v := range vectors {
		got, err := TOTPCode(rfcSecret, CounterAt(time.Unix(v.unix, 0)))
		if err != nil {
			t.Fatal(err)
		}
		if got != v.want {
			t.Errorf("T=%d: got %s, want %s", v.unix, got, v.want)
		}
	}
}

func TestVerifyTOTPAcceptsOneStepOfSkew(t *testing.T) {
	at := time.Unix(1111111111, 0)
	now := CounterAt(at)
	for _, delta := range []int64{-1, 0, 1} {
		code, _ := TOTPCode(rfcSecret, uint64(int64(now)+delta))
		counter, err := VerifyTOTP(rfcSecret, code, at, nil)
		if err != nil {
			t.Errorf("delta %d rejected: %v", delta, err)
		}
		if counter != uint64(int64(now)+delta) {
			t.Errorf("delta %d: returned counter %d", delta, counter)
		}
	}
	code, _ := TOTPCode(rfcSecret, now+2)
	if _, err := VerifyTOTP(rfcSecret, code, at, nil); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("two steps ahead should be rejected, got %v", err)
	}
}

func TestVerifyTOTPRefusesReplay(t *testing.T) {
	at := time.Unix(1234567890, 0)
	code, _ := TOTPCode(rfcSecret, CounterAt(at))
	counter, err := VerifyTOTP(rfcSecret, code, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same code, same thirty seconds, after it has been accepted once.
	if _, err := VerifyTOTP(rfcSecret, code, at, &counter); !errors.Is(err, ErrCodeReplayed) {
		t.Fatalf("replayed code: got %v, want ErrCodeReplayed", err)
	}
	// The next step's code is still fine.
	next, _ := TOTPCode(rfcSecret, counter+1)
	if _, err := VerifyTOTP(rfcSecret, next, at, &counter); err != nil {
		t.Fatalf("next code rejected: %v", err)
	}
}

func TestVerifyTOTPRejectsJunk(t *testing.T) {
	at := time.Unix(59, 0)
	for _, code := range []string{"", "12345", "1234567", "abcdef", "28708２"} {
		if _, err := VerifyTOTP(rfcSecret, code, at, nil); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("VerifyTOTP(%q) = %v, want ErrInvalidCode", code, err)
		}
	}
	if _, err := VerifyTOTP("not base32!!", "287082", at, nil); err == nil {
		t.Error("malformed secret accepted")
	}
	// Spaces are tolerated: people type "287 082".
	if _, err := VerifyTOTP(rfcSecret, "287 082", at, nil); err != nil {
		t.Errorf("spaced code rejected: %v", err)
	}
}

func TestProvisioningURI(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 || strings.Contains(secret, "=") {
		t.Fatalf("secret should be 32 unpadded base32 characters, got %q", secret)
	}
	uri := TOTPProvisioningURI("Medical Expert System - Greenside", "thandi@greenside.co.za", secret)
	for _, want := range []string{"otpauth://totp/", "secret=" + secret, "issuer=Medical+Expert+System", "digits=6", "period=30", "algorithm=SHA1"} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI %q missing %q", uri, want)
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := NewRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 19 || c[4] != '-' || c[9] != '-' || c[14] != '-' {
			t.Errorf("unexpected format %q", c)
		}
		if strings.ContainsAny(c, "IO01") {
			t.Errorf("code %q contains an ambiguous character", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
	}
	// What a person types off a printout must hash the same.
	c := codes[0]
	typed := strings.ToLower(strings.ReplaceAll(c, "-", " "))
	if HashRecoveryCode(c) != HashRecoveryCode(typed) {
		t.Error("normalisation failed: lower-case, spaced input hashes differently")
	}
}
