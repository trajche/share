package files

import (
	"mime"
	"strings"

	"github.com/tus/tusd/v2/pkg/handler"
)

// Presentation describes how a stored file is sent to the client.
type Presentation struct {
	ContentType        string
	ContentDisposition string
	// Inline reports whether the browser should render the file itself.
	Inline bool
	// ContentSecurityPolicy is applied to every download. Files are served
	// from the same origin as the web UI, so anything the browser renders
	// must be inert.
	ContentSecurityPolicy string
}

// sandboxCSP makes a rendered file an opaque-origin document with no script
// execution, forms, or external requests. It is used for everything that
// could carry script (SVG, text shown as plain text, downloads).
const sandboxCSP = "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox"

// mediaCSP is used for raster images, audio and video. These cannot run
// script, and the browser's media document must stay same-origin: from an
// opaque (sandboxed) origin the player's own range requests are cross-site,
// so the SameSite unlock cookie of password-protected files is not sent.
const mediaCSP = "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'"

// pdfCSP omits the sandbox directive, which stops the built-in PDF viewers
// in Chrome and Firefox from loading.
const pdfCSP = "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; object-src 'self'"

// inlineMedia lists types the browser may render as-is.
var inlineMedia = map[string]bool{
	"image/png":     true,
	"image/jpeg":    true,
	"image/gif":     true,
	"image/webp":    true,
	"image/avif":    true,
	"image/bmp":     true,
	"image/x-icon":  true,
	"image/svg+xml": true, // scripts are disabled by sandboxCSP

	"video/mp4":       true,
	"video/webm":      true,
	"video/ogg":       true,
	"video/quicktime": true,

	"audio/mpeg":  true,
	"audio/mp4":   true,
	"audio/aac":   true,
	"audio/ogg":   true,
	"audio/opus":  true,
	"audio/flac":  true,
	"audio/wav":   true,
	"audio/x-wav": true,
	"audio/wave":  true,
	"audio/webm":  true,

	"application/ogg":  true,
	"application/pdf":  true,
	"application/json": true,
}

// textLike lists non-text/* types whose contents are shown as plain text
// when previewed.
var textLike = map[string]bool{
	"application/xml":        true,
	"application/javascript": true,
	"application/x-yaml":     true,
	"application/yaml":       true,
	"application/toml":       true,
	"application/x-sh":       true,
	"application/sql":        true,
	"application/xhtml+xml":  true,
}

// FileType returns the MIME type recorded for an upload, or "" if unknown.
func FileType(meta handler.MetaData) string {
	if ft := meta[MetaFiletype]; ft != "" {
		return ft
	}
	return meta[MetaContentType]
}

// Present decides the Content-Type, Content-Disposition and CSP for a
// download. Safe media (images, video, audio, PDF) is shown inline; text,
// markup and code are shown inline as plain text (UTF-8 unless the upload
// named a charset) so they cannot execute; everything else is downloaded. forceDownload (from ?dl=1) and an
// upload-time "disposition: attachment" both force a download.
func Present(meta handler.MetaData, forceDownload bool) Presentation {
	mediaType, params, err := mime.ParseMediaType(FileType(meta))
	contentType := mime.FormatMediaType(mediaType, params)
	if err != nil || contentType == "" {
		mediaType, params = "application/octet-stream", nil
		contentType = mediaType
	}

	p := Presentation{ContentType: contentType, ContentSecurityPolicy: sandboxCSP}
	inline := !forceDownload && meta[MetaDisposition] != DispositionAttachment

	switch {
	case inlineMedia[mediaType]:
		switch {
		case mediaType == "application/pdf":
			p.ContentSecurityPolicy = pdfCSP
		case mediaType == "image/svg+xml", mediaType == "application/json":
			// Keep the sandbox: SVG can contain script.
		default:
			p.ContentSecurityPolicy = mediaCSP
		}
	case strings.HasPrefix(mediaType, "text/") || textLike[mediaType]:
		if inline {
			charset := params["charset"]
			if charset == "" {
				charset = "utf-8"
			}
			p.ContentType = mime.FormatMediaType("text/plain", map[string]string{"charset": charset})
		}
	default:
		inline = false
	}

	disposition := DispositionAttachment
	if inline {
		disposition = DispositionInline
	}
	p.Inline = inline
	p.ContentDisposition = disposition
	if name := meta[MetaFilename]; name != "" {
		if v := mime.FormatMediaType(disposition, map[string]string{"filename": name}); v != "" {
			p.ContentDisposition = v
		}
	}
	return p
}
