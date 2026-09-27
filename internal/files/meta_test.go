package files

import (
	"testing"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

func TestParseExpiry(t *testing.T) {
	cases := map[string]time.Duration{
		"":    24 * time.Hour,
		"1h":  time.Hour,
		"6h":  6 * time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseExpiry(in)
		if err != nil || got != want {
			t.Errorf("ParseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"2h", "1y", "24H", "-1h"} {
		if _, err := ParseExpiry(bad); err == nil {
			t.Errorf("ParseExpiry(%q) accepted", bad)
		}
	}
}

func TestMetadataFiltering(t *testing.T) {
	meta := handler.MetaData{
		MetaFilename:     "a.txt",
		MetaPassword:     "pw",
		MetaPasswordHash: "h",
		MetaTokenHash:    "t",
		MetaLegacyToken:  "l",
	}

	public := PublicMetadata(meta)
	if len(public) != 1 || public[MetaFilename] != "a.txt" {
		t.Errorf("PublicMetadata = %v", public)
	}
	if len(meta) != 5 {
		t.Error("PublicMetadata modified its input")
	}

	StripServerOwned(meta)
	for _, k := range []string{MetaPasswordHash, MetaTokenHash, MetaLegacyToken} {
		if _, ok := meta[k]; ok {
			t.Errorf("StripServerOwned kept %q", k)
		}
	}
	if meta[MetaPassword] != "pw" || meta[MetaFilename] != "a.txt" {
		t.Error("StripServerOwned removed client keys")
	}
}

func TestSplitID(t *testing.T) {
	cases := []struct {
		id, obj, mp string
		ok          bool
	}{
		{"abc123+mp.id~x", "abc123", "mp.id~x", true},
		{"f81d4fae-7dec-11d0-a765-00a0c91e6bf6+mcp", "f81d4fae-7dec-11d0-a765-00a0c91e6bf6", "mcp", true},
		{"abc123", "abc123", "", true},
		{"", "", "", false},
		{"../secret+x", "../secret", "x", false},
		{"a/b", "a/b", "", false},
		{"a.info", "a.info", "", false},
	}
	for _, c := range cases {
		obj, mp, ok := SplitID(c.id)
		if obj != c.obj || mp != c.mp || ok != c.ok {
			t.Errorf("SplitID(%q) = %q, %q, %v; want %q, %q, %v", c.id, obj, mp, ok, c.obj, c.mp, c.ok)
		}
	}
}

func TestValidDisposition(t *testing.T) {
	for _, d := range []string{"", "inline", "attachment"} {
		if !ValidDisposition(d) {
			t.Errorf("%q rejected", d)
		}
	}
	if ValidDisposition("download") {
		t.Error(`"download" accepted`)
	}
}
