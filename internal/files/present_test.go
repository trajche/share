package files

import (
	"strings"
	"testing"

	"github.com/tus/tusd/v2/pkg/handler"
)

func TestPresent(t *testing.T) {
	tests := []struct {
		name        string
		meta        handler.MetaData
		download    bool
		wantType    string
		wantDispo   string
		wantSandbox bool
	}{
		{"image inline", handler.MetaData{"filetype": "image/png", "filename": "a.png"}, false, "image/png", `inline; filename=a.png`, false},
		{"video inline", handler.MetaData{"filetype": "video/mp4"}, false, "video/mp4", "inline", false},
		{"audio inline", handler.MetaData{"filetype": "audio/mpeg"}, false, "audio/mpeg", "inline", false},
		{"pdf inline without sandbox", handler.MetaData{"filetype": "application/pdf"}, false, "application/pdf", "inline", false},
		{"svg inline sandboxed", handler.MetaData{"filetype": "image/svg+xml"}, false, "image/svg+xml", "inline", true},
		{"plain text gets utf-8", handler.MetaData{"filetype": "text/plain"}, false, "text/plain; charset=utf-8", "inline", true},
		{"plain text keeps charset", handler.MetaData{"filetype": "text/plain; charset=windows-1251"}, false, "text/plain; charset=windows-1251", "inline", true},
		{"csv shown as text", handler.MetaData{"filetype": "text/csv"}, false, "text/plain; charset=utf-8", "inline", true},
		{"json inline sandboxed", handler.MetaData{"filetype": "application/json"}, false, "application/json", "inline", true},
		{"html shown as text", handler.MetaData{"filetype": "text/html"}, false, "text/plain; charset=utf-8", "inline", true},
		{"html keeps charset", handler.MetaData{"filetype": "text/html; charset=iso-8859-1"}, false, "text/plain; charset=iso-8859-1", "inline", true},
		{"xml shown as text", handler.MetaData{"filetype": "application/xml"}, false, "text/plain; charset=utf-8", "inline", true},
		{"html forced download keeps type", handler.MetaData{"filetype": "text/html"}, true, "text/html", "attachment", true},
		{"content-type key fallback", handler.MetaData{"content-type": "image/jpeg"}, false, "image/jpeg", "inline", false},
		{"unknown type downloads", handler.MetaData{"filetype": "application/zip"}, false, "application/zip", "attachment", true},
		{"missing type downloads", handler.MetaData{}, false, "application/octet-stream", "attachment", true},
		{"malformed type downloads", handler.MetaData{"filetype": "not a type"}, false, "application/octet-stream", "attachment", true},
		{"dl=1 forces download", handler.MetaData{"filetype": "image/png"}, true, "image/png", "attachment", false},
		{"disposition attachment", handler.MetaData{"filetype": "image/png", "disposition": "attachment"}, false, "image/png", "attachment", false},
		{"unicode filename encoded", handler.MetaData{"filetype": "image/png", "filename": "слика.png"}, false, "image/png", "inline; filename*=utf-8''%D1%81%D0%BB%D0%B8%D0%BA%D0%B0.png", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Present(tt.meta, tt.download)
			if p.ContentType != tt.wantType {
				t.Errorf("ContentType = %q, want %q", p.ContentType, tt.wantType)
			}
			if p.ContentDisposition != tt.wantDispo {
				t.Errorf("ContentDisposition = %q, want %q", p.ContentDisposition, tt.wantDispo)
			}
			if got := strings.Contains(p.ContentSecurityPolicy, "sandbox"); got != tt.wantSandbox {
				t.Errorf("sandbox in CSP = %v, want %v (%q)", got, tt.wantSandbox, p.ContentSecurityPolicy)
			}
			if !strings.Contains(p.ContentSecurityPolicy, "default-src 'none'") {
				t.Errorf("CSP missing default-src 'none': %q", p.ContentSecurityPolicy)
			}
		})
	}
}

func TestPresentRejectsHeaderInjection(t *testing.T) {
	p := Present(handler.MetaData{"filetype": "image/png\r\nSet-Cookie: x=y"}, false)
	if strings.ContainsAny(p.ContentType, "\r\n") {
		t.Fatalf("content type contains newline: %q", p.ContentType)
	}
}
