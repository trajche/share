package files

import (
	"strings"
	"testing"

	"github.com/tus/tusd/v2/pkg/handler"
)

func TestTokenMatches(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64", len(token))
	}

	hashed := handler.MetaData{MetaTokenHash: HashToken(token)}
	legacy := handler.MetaData{MetaLegacyToken: token}

	for name, meta := range map[string]handler.MetaData{"hashed": hashed, "legacy": legacy} {
		if !TokenMatches(meta, token) {
			t.Errorf("%s: correct token rejected", name)
		}
		if TokenMatches(meta, strings.Repeat("0", 64)) {
			t.Errorf("%s: wrong token accepted", name)
		}
		if TokenMatches(meta, "") {
			t.Errorf("%s: empty token accepted", name)
		}
	}
	if TokenMatches(handler.MetaData{}, token) {
		t.Error("token accepted for upload without a stored token")
	}
	// A stored hash must not be usable as the token itself.
	if TokenMatches(hashed, hashed[MetaTokenHash]) {
		t.Error("stored hash accepted as token")
	}
}

func TestPassword(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$600000$") {
		t.Fatalf("unexpected hash format %q", hash)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("hash contains the password")
	}
	if !VerifyPassword(hash, "correct horse") {
		t.Error("correct password rejected")
	}
	for _, pw := range []string{"", "correct horse ", "Correct horse", strings.Repeat("x", MaxPasswordLength+1)} {
		if VerifyPassword(hash, pw) {
			t.Errorf("wrong password %q accepted", pw)
		}
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$x$a$b", "md5$1$a$b"} {
		if VerifyPassword(bad, "correct horse") {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}

	again, _ := HashPassword("correct horse")
	if again == hash {
		t.Error("hashes are not salted")
	}

	if _, err := HashPassword(""); err == nil {
		t.Error("empty password accepted")
	}
	if _, err := HashPassword(strings.Repeat("x", MaxPasswordLength+1)); err == nil {
		t.Error("overlong password accepted")
	}
}

func TestUnlockValue(t *testing.T) {
	hash, _ := HashPassword("pw")
	v := UnlockValue(hash, "obj1")
	if !VerifyUnlock(hash, "obj1", v) {
		t.Error("valid unlock value rejected")
	}
	if VerifyUnlock(hash, "obj2", v) {
		t.Error("unlock value accepted for another file")
	}
	other, _ := HashPassword("pw")
	if VerifyUnlock(other, "obj1", v) {
		t.Error("unlock value survived a password change")
	}
	if VerifyUnlock(hash, "obj1", "") {
		t.Error("empty unlock value accepted")
	}
}
