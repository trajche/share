// Package files holds the upload model shared by the tus hooks, the HTTP
// download/management endpoints, and the MCP server: metadata keys, expiry
// options, management tokens, password hashing, and the inline/download
// presentation policy.
package files

import (
	"fmt"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

// Upload metadata keys.
const (
	MetaFilename    = "filename"
	MetaFiletype    = "filetype"
	MetaContentType = "content-type" // alternative to filetype sent by some clients
	MetaExpiresIn   = "expires-in"
	MetaDisposition = "disposition" // "inline" (default) or "attachment"

	// MetaPassword is accepted from clients at creation time only. It is
	// replaced by MetaPasswordHash before the upload is stored.
	MetaPassword     = "password"
	MetaPasswordHash = "password-hash"

	// MetaTokenHash stores the SHA-256 of the management token.
	MetaTokenHash = "mgmt-token-hash"
	// MetaLegacyToken stores a plaintext management token (MCP uploads made
	// before token hashing was introduced).
	MetaLegacyToken = "mgmt-token"
)

// serverOwnedKeys may only be set by the server; client-supplied values are
// discarded at creation time.
var serverOwnedKeys = []string{MetaPasswordHash, MetaTokenHash, MetaLegacyToken}

// sensitiveKeys must never be exposed in responses (e.g. the tus HEAD
// Upload-Metadata header).
var sensitiveKeys = []string{MetaPassword, MetaPasswordHash, MetaTokenHash, MetaLegacyToken}

// StripServerOwned removes metadata keys that clients are not allowed to set.
func StripServerOwned(meta handler.MetaData) {
	for _, k := range serverOwnedKeys {
		delete(meta, k)
	}
}

// PublicMetadata returns a copy of meta without secret values.
func PublicMetadata(meta handler.MetaData) handler.MetaData {
	out := make(handler.MetaData, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	for _, k := range sensitiveKeys {
		delete(out, k)
	}
	return out
}

// Dispositions accepted in the "disposition" metadata field.
const (
	DispositionInline     = "inline"
	DispositionAttachment = "attachment"
)

// ValidDisposition reports whether d is an accepted disposition value.
// The empty string means "inline where safe".
func ValidDisposition(d string) bool {
	return d == "" || d == DispositionInline || d == DispositionAttachment
}

// DefaultExpiry is used when the client does not choose one.
const DefaultExpiry = "24h"

// ExpiryChoices lists the valid expiry values for error messages and docs.
const ExpiryChoices = "1h, 6h, 24h, 7d, 30d"

var expiries = map[string]time.Duration{
	"1h":  1 * time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// ParseExpiry resolves an expires-in value; empty means DefaultExpiry.
func ParseExpiry(s string) (time.Duration, error) {
	if s == "" {
		s = DefaultExpiry
	}
	d, ok := expiries[s]
	if !ok {
		return 0, fmt.Errorf("invalid expires-in %q; valid values: %s", s, ExpiryChoices)
	}
	return d, nil
}
