package files

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tus/tusd/v2/pkg/handler"
)

// NewToken returns a random 256-bit management token as 64 hex characters.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewObjectID returns a random 128-bit object ID as 32 hex characters.
func NewObjectID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashToken returns the stored form of a management token. Tokens carry
// 256 bits of entropy, so a single SHA-256 is sufficient.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TokenMatches reports whether provided is the management token for an
// upload with the given metadata. Comparison is constant-time.
func TokenMatches(meta handler.MetaData, provided string) bool {
	if provided == "" {
		return false
	}
	if stored := meta[MetaTokenHash]; stored != "" {
		return subtle.ConstantTimeCompare([]byte(stored), []byte(HashToken(provided))) == 1
	}
	if stored := meta[MetaLegacyToken]; stored != "" {
		return subtle.ConstantTimeCompare([]byte(stored), []byte(provided)) == 1
	}
	return false
}

// MaxPasswordLength bounds user passwords to keep hashing cost predictable.
const MaxPasswordLength = 256

// pbkdf2Iterations follows the OWASP recommendation for PBKDF2-HMAC-SHA256.
// It is a variable so tests can lower it.
var pbkdf2Iterations = 600_000

// verifySem bounds concurrent password hashing so unlock attempts cannot
// monopolise the CPU.
var verifySem = make(chan struct{}, 4)

// HashPassword returns an encoded PBKDF2-SHA256 hash of pw in the form
// "pbkdf2-sha256$<iterations>$<salt>$<key>".
func HashPassword(pw string) (string, error) {
	if pw == "" {
		return "", errors.New("password must not be empty")
	}
	if len(pw) > MaxPasswordLength {
		return "", fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derive(pw, salt, pbkdf2Iterations)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the encoded hash.
func VerifyPassword(encoded, pw string) bool {
	if pw == "" || len(pw) > MaxPasswordLength {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := derive(pw, salt, iter)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func derive(pw string, salt []byte, iter int) ([]byte, error) {
	verifySem <- struct{}{}
	defer func() { <-verifySem }()
	return pbkdf2.Key(sha256.New, pw, salt, iter, 32)
}

// UnlockValue returns the cookie value that proves a browser has entered the
// correct password for objectID. It is keyed by the stored password hash, so
// changing the password or re-uploading invalidates it, and it never needs a
// server-wide secret.
func UnlockValue(passwordHash, objectID string) string {
	mac := hmac.New(sha256.New, []byte(passwordHash))
	mac.Write([]byte("unlock:" + objectID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyUnlock checks a cookie value produced by UnlockValue.
func VerifyUnlock(passwordHash, objectID, value string) bool {
	if value == "" {
		return false
	}
	return hmac.Equal([]byte(UnlockValue(passwordHash, objectID)), []byte(value))
}
