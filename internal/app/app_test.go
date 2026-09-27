package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/files"
	"sharemk/internal/hooks"
	"sharemk/internal/testutil"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	s3     *s3.Client
	client *http.Client // does not follow redirects
}

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s3Client, cfg := testutil.NewS3(t)
	a, err := New(cfg, s3Client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.ProcessCompletions(ctx)

	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return &env{
		t:   t,
		srv: srv,
		s3:  s3Client,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

type upload struct {
	location, id, objectID, token, manageURL string
}

// create starts a tus upload and returns the raw response.
func (e *env) create(size int, meta map[string]string) *http.Response {
	e.t.Helper()
	var pairs []string
	for k, v := range meta {
		pairs = append(pairs, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/files/", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(size))
	req.Header.Set("Upload-Metadata", strings.Join(pairs, ","))
	return e.do(req)
}

// upload creates a tus upload and sends content in one PATCH.
func (e *env) upload(content string, meta map[string]string) upload {
	e.t.Helper()
	u := e.start(len(content), meta)
	req, _ := http.NewRequest(http.MethodPatch, u.location, strings.NewReader(content))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	if resp := e.do(req); resp.StatusCode != http.StatusNoContent {
		e.t.Fatalf("patch: status %d: %s", resp.StatusCode, readBody(resp))
	}
	return u
}

// start creates a tus upload of the given size without sending any data.
func (e *env) start(size int, meta map[string]string) upload {
	e.t.Helper()
	resp := e.create(size, meta)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create: status %d: %s", resp.StatusCode, readBody(resp))
	}
	u := upload{
		location:  resp.Header.Get("Location"),
		token:     resp.Header.Get(hooks.HeaderManagementToken),
		manageURL: resp.Header.Get(hooks.HeaderManageURL),
	}
	u.id = path.Base(u.location)
	u.objectID, _, _ = files.SplitID(u.id)
	return u
}

func (e *env) do(req *http.Request) *http.Response {
	e.t.Helper()
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) get(u string, header ...string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return e.do(req)
}

func (e *env) bucketKeys() []string {
	e.t.Helper()
	out, err := e.s3.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(testutil.Bucket)})
	if err != nil {
		e.t.Fatal(err)
	}
	var keys []string
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	return keys
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, want, readBody(resp))
	}
}

func TestPreviewAndDownload(t *testing.T) {
	e := newEnv(t)

	html := e.upload("<script>alert(1)</script>", map[string]string{"filename": "x.html", "filetype": "text/html"})
	resp := e.get(html.location)
	expectStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("html Content-Type = %q, want text/plain", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "inline; filename=x.html" {
		t.Errorf("html Content-Disposition = %q", cd)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("CSP = %q, want sandbox", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if body := readBody(resp); body != "<script>alert(1)</script>" {
		t.Errorf("body = %q", body)
	}

	resp = e.get(html.location + "?dl=1")
	expectStatus(t, resp, http.StatusOK)
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("?dl=1 Content-Disposition = %q", cd)
	}

	img := e.upload("PNGDATA0123456789", map[string]string{"filename": "a.png", "filetype": "image/png"})
	resp = e.get(img.location)
	expectStatus(t, resp, http.StatusOK)
	if ct, cd := resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"); ct != "image/png" || !strings.HasPrefix(cd, "inline") {
		t.Errorf("png served as %q / %q", ct, cd)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("missing Accept-Ranges")
	}

	forced := e.upload("x", map[string]string{"filetype": "image/png", "disposition": "attachment"})
	if cd := e.get(forced.location).Header.Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("disposition=attachment served as %q", cd)
	}

	bin := e.upload("PK", map[string]string{"filetype": "application/zip"})
	if cd := e.get(bin.location).Header.Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("zip served as %q", cd)
	}
}

func TestRangeRequests(t *testing.T) {
	e := newEnv(t)
	u := e.upload("0123456789", map[string]string{"filetype": "video/mp4"})

	resp := e.get(u.location, "Range", "bytes=2-5")
	expectStatus(t, resp, http.StatusPartialContent)
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q", cr)
	}
	if body := readBody(resp); body != "2345" {
		t.Errorf("body = %q", body)
	}

	etag := e.get(u.location).Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	expectStatus(t, e.get(u.location, "If-None-Match", etag), http.StatusNotModified)
}

func TestShortIDAndIncompleteUpload(t *testing.T) {
	e := newEnv(t)
	u := e.upload("hello", map[string]string{"filetype": "text/plain"})

	resp := e.get(e.srv.URL + "/files/" + u.objectID)
	expectStatus(t, resp, http.StatusOK)
	if body := readBody(resp); body != "hello" {
		t.Errorf("body = %q", body)
	}

	pending := e.start(100, map[string]string{"filetype": "text/plain", "filename": "later.txt"})
	expectStatus(t, e.get(pending.location), http.StatusConflict)

	expectStatus(t, e.get(e.srv.URL+"/files/does-not-exist"), http.StatusNotFound)
	expectStatus(t, e.get(e.srv.URL+"/files/..%2F..%2Fetc"), http.StatusNotFound)
}

func TestHeadDoesNotLeakSecrets(t *testing.T) {
	e := newEnv(t)
	u := e.upload("x", map[string]string{"filename": "a.txt", "password": "pw"})

	req, _ := http.NewRequest(http.MethodHead, u.location, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	resp := e.do(req)
	expectStatus(t, resp, http.StatusOK)

	meta := handler.ParseMetadataHeader(resp.Header.Get("Upload-Metadata"))
	if meta["filename"] != "a.txt" {
		t.Errorf("public metadata missing: %v", meta)
	}
	for _, k := range []string{files.MetaPassword, files.MetaPasswordHash, files.MetaTokenHash, files.MetaLegacyToken} {
		if _, ok := meta[k]; ok {
			t.Errorf("HEAD exposes %q", k)
		}
	}
	if resp.Header.Get("Upload-Offset") != "1" {
		t.Errorf("Upload-Offset = %q", resp.Header.Get("Upload-Offset"))
	}
}

func TestCreateReturnsManagementToken(t *testing.T) {
	e := newEnv(t)
	u := e.upload("x", nil)
	if len(u.token) != 64 {
		t.Fatalf("token = %q", u.token)
	}
	if want := "https://share.test/manage/" + u.objectID + "#" + u.token; u.manageURL != want {
		t.Errorf("manage URL = %q, want %q", u.manageURL, want)
	}

	// CORS clients must be able to read the new headers.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/files/", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "1")
	req.Header.Set("Origin", "https://example.com")
	expose := e.do(req).Header.Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, hooks.HeaderManagementToken) || !strings.Contains(expose, hooks.HeaderManageURL) {
		t.Errorf("Access-Control-Expose-Headers = %q", expose)
	}
}

func TestInvalidMetadataCreatesNothing(t *testing.T) {
	e := newEnv(t)
	for _, meta := range []map[string]string{
		{"expires-in": "99y"},
		{"disposition": "bogus"},
		{"password": strings.Repeat("x", files.MaxPasswordLength+1)},
	} {
		resp := e.create(3, meta)
		expectStatus(t, resp, http.StatusBadRequest)
		if resp.Header.Get("Location") != "" {
			t.Errorf("%v: Location set on rejected upload", meta)
		}
	}
	if keys := e.bucketKeys(); len(keys) != 0 {
		t.Errorf("rejected uploads left objects: %v", keys)
	}
}

func TestDeleteRequiresToken(t *testing.T) {
	e := newEnv(t)
	u := e.upload("data", map[string]string{"filetype": "text/plain"})
	other := e.upload("data", nil)

	del := func(target string, header ...string) *http.Response {
		req, _ := http.NewRequest(http.MethodDelete, target, nil)
		req.Header.Set("Tus-Resumable", "1.0.0")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		return e.do(req)
	}

	expectStatus(t, del(u.location), http.StatusUnauthorized)
	expectStatus(t, del(u.location, "Authorization", "Bearer wrong"), http.StatusNotFound)
	expectStatus(t, del(u.location, "Authorization", "Bearer "+other.token), http.StatusNotFound)
	expectStatus(t, e.get(u.location), http.StatusOK)

	// The header form is accepted too, and the short ID works.
	expectStatus(t, del(e.srv.URL+"/files/"+u.objectID, hooks.HeaderManagementToken, u.token), http.StatusNoContent)
	expectStatus(t, e.get(u.location), http.StatusNotFound)
	for _, k := range e.bucketKeys() {
		if strings.Contains(k, u.objectID) {
			t.Errorf("object %s left after delete", k)
		}
	}

	expectStatus(t, del(e.srv.URL+"/api/files/"+other.id, "Authorization", "Bearer "+other.token), http.StatusNoContent)
	expectStatus(t, e.get(other.location), http.StatusNotFound)
}

func TestDeleteIncompleteUpload(t *testing.T) {
	e := newEnv(t)
	u := e.start(100, map[string]string{"filename": "big.bin"})

	req, _ := http.NewRequest(http.MethodDelete, e.srv.URL+"/api/files/"+u.id, nil)
	req.Header.Set("Authorization", "Bearer "+u.token)
	expectStatus(t, e.do(req), http.StatusNoContent)

	if keys := e.bucketKeys(); len(keys) != 0 {
		t.Errorf("objects left: %v", keys)
	}
}

func TestInfoAPI(t *testing.T) {
	e := newEnv(t)
	u := e.upload("hello", map[string]string{"filename": "a.txt", "filetype": "text/plain", "expires-in": "1h"})

	expectStatus(t, e.get(e.srv.URL+"/api/files/"+u.id), http.StatusUnauthorized)
	expectStatus(t, e.get(e.srv.URL+"/api/files/"+u.id, "Authorization", "Bearer x"), http.StatusNotFound)

	var info map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := e.get(e.srv.URL+"/api/files/"+u.objectID, "Authorization", "Bearer "+u.token)
		expectStatus(t, resp, http.StatusOK)
		info = nil
		json.NewDecoder(resp.Body).Decode(&info) //nolint:errcheck
		if info["expires_at"] != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond) // tagging runs asynchronously
	}

	want := map[string]any{
		"file_id": u.id, "filename": "a.txt", "content_type": "text/plain", "size_bytes": 5.0,
		"complete": true, "download_url": "https://share.test/files/" + u.objectID + "/a.txt",
		"disposition": "inline", "password_protected": false,
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("%s = %v, want %v", k, info[k], v)
		}
	}
	at, err := time.Parse(time.RFC3339, info["expires_at"].(string))
	if err != nil {
		t.Fatalf("expires_at = %v", info["expires_at"])
	}
	if d := time.Until(at); d < 55*time.Minute || d > time.Hour+time.Minute {
		t.Errorf("expires in %v, want ~1h", d)
	}
	for _, k := range []string{"management_token", "mgmt-token", "password_hash"} {
		if _, ok := info[k]; ok {
			t.Errorf("info exposes %q", k)
		}
	}
}

func TestPasswordProtection(t *testing.T) {
	e := newEnv(t)
	u := e.upload("top secret", map[string]string{"filename": "s.txt", "filetype": "text/plain", "password": "hunter2"})
	other := e.upload("other", map[string]string{"filetype": "text/plain", "password": "hunter2"})

	// API client without credentials gets JSON.
	resp := e.get(u.location)
	expectStatus(t, resp, http.StatusUnauthorized)
	if body := readBody(resp); strings.Contains(body, "top secret") || !strings.Contains(body, "password") {
		t.Errorf("body = %q", body)
	}

	// Browsers get the form.
	resp = e.get(u.location, "Accept", "text/html")
	expectStatus(t, resp, http.StatusUnauthorized)
	if body := readBody(resp); !strings.Contains(body, `name="password"`) || !strings.Contains(body, `action="/files/`+strings.ReplaceAll(u.id, "+", "&#43;")+`"`) {
		t.Errorf("unlock form missing: %s", body)
	}

	// Basic auth.
	req, _ := http.NewRequest(http.MethodGet, u.location, nil)
	req.SetBasicAuth("", "wrong")
	expectStatus(t, e.do(req), http.StatusUnauthorized)
	req, _ = http.NewRequest(http.MethodGet, u.location, nil)
	req.SetBasicAuth("anyone", "hunter2")
	resp = e.do(req)
	expectStatus(t, resp, http.StatusOK)
	if body := readBody(resp); body != "top secret" {
		t.Errorf("body = %q", body)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}

	// Form unlock sets a cookie scoped to this file.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: e.client.CheckRedirect}
	post := func(pw, dl string) *http.Response {
		form := url.Values{"password": {pw}}
		if dl != "" {
			form.Set("dl", dl)
		}
		req, _ := http.NewRequest(http.MethodPost, u.location, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "text/html")
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	resp = post("nope", "")
	expectStatus(t, resp, http.StatusUnauthorized)
	if body := readBody(resp); !strings.Contains(body, "Wrong password") {
		t.Error("wrong-password message missing")
	}

	resp = post("hunter2", "1")
	expectStatus(t, resp, http.StatusSeeOther)
	if loc := resp.Header.Get("Location"); loc != "/files/"+u.id+"?dl=1" {
		t.Errorf("redirect = %q", loc)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		cookie = c
	}
	if cookie == nil || !cookie.HttpOnly || cookie.Path != "/files/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unlock cookie = %+v", cookie)
	}

	resp, err := browser.Get(u.location)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)

	// The cookie does not unlock another file with the same password.
	resp, err = browser.Get(other.location)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusUnauthorized)

	// Management still uses the token, not the password.
	resp = e.get(e.srv.URL+"/api/files/"+u.id, "Authorization", "Bearer "+u.token)
	expectStatus(t, resp, http.StatusOK)
	if body := readBody(resp); !strings.Contains(body, `"password_protected":true`) {
		t.Errorf("info = %s", body)
	}
}

func TestPasswordFormOnPublicFileRedirects(t *testing.T) {
	e := newEnv(t)
	u := e.upload("x", nil)
	req, _ := http.NewRequest(http.MethodPost, u.location, strings.NewReader("password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := e.do(req)
	expectStatus(t, resp, http.StatusSeeOther)
}

func TestManagePage(t *testing.T) {
	e := newEnv(t)
	resp := e.get(e.srv.URL + "/manage/abc")
	expectStatus(t, resp, http.StatusOK)
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Error("manage page must not leak its URL via Referer")
	}
	expectStatus(t, e.get(e.srv.URL+"/ui/page.css"), http.StatusOK)
}

// mcpSession speaks JSON-RPC to the MCP Streamable HTTP endpoint.
type mcpSession struct {
	e   *env
	sid string
	id  int
}

func (e *env) mcp() *mcpSession {
	s := &mcpSession{e: e}
	resp := s.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	s.sid = resp.Header.Get("Mcp-Session-Id")
	if s.sid == "" {
		e.t.Fatal("no MCP session id")
	}
	s.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return s
}

func (s *mcpSession) post(body string) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, s.e.srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if s.sid != "" {
		req.Header.Set("Mcp-Session-Id", s.sid)
	}
	return s.e.do(req)
}

// call invokes a tool and returns its text result and error flag.
func (s *mcpSession) call(tool string, args map[string]any) (map[string]any, string) {
	s.e.t.Helper()
	s.id++
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": s.id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	resp := s.post(string(body))
	expectStatus(s.e.t, resp, http.StatusOK)

	var rpc struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil || len(rpc.Result.Content) == 0 {
		s.e.t.Fatalf("bad MCP response: %v", err)
	}
	text := rpc.Result.Content[0].Text
	if rpc.Result.IsError {
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		s.e.t.Fatalf("tool result is not JSON: %q", text)
	}
	return out, ""
}

func TestMCPTools(t *testing.T) {
	e := newEnv(t)
	m := e.mcp()

	res, errText := m.call("upload_file", map[string]any{
		"filename":     "clip.mp4",
		"content":      base64.StdEncoding.EncodeToString([]byte("fake video")),
		"content_type": "video/mp4",
		"expires_in":   "7d",
		"password":     "pw",
	})
	if errText != "" {
		t.Fatal(errText)
	}
	id, token := res["file_id"].(string), res["management_token"].(string)
	objectID, _, _ := files.SplitID(id)
	if res["password_protected"] != true || res["manage_url"] != "https://share.test/manage/"+objectID+"#"+token {
		t.Errorf("upload result = %v", res)
	}

	if want := "https://share.test/files/" + objectID + "/clip.mp4"; res["download_url"] != want {
		t.Errorf("download_url = %v, want %v", res["download_url"], want)
	}

	// The MCP upload is served by the normal download path.
	download := e.srv.URL + "/files/" + id
	expectStatus(t, e.get(download), http.StatusUnauthorized)
	req, _ := http.NewRequest(http.MethodGet, download, nil)
	req.SetBasicAuth("", "pw")
	resp := e.do(req)
	expectStatus(t, resp, http.StatusOK)
	if ct, body := resp.Header.Get("Content-Type"), readBody(resp); ct != "video/mp4" || body != "fake video" {
		t.Errorf("download = %q %q", ct, body)
	}

	// Only the hash of the token is stored.
	out, err := e.s3.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testutil.Bucket), Key: aws.String("uploads/" + objectID + ".info"),
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if strings.Contains(string(stored), token) || strings.Contains(string(stored), `"pw"`) {
		t.Error(".info contains plaintext secrets")
	}

	info, errText := m.call("get_file_info", map[string]any{"file_id": id, "management_token": token})
	if errText != "" {
		t.Fatal(errText)
	}
	if info["complete"] != true || info["password_protected"] != true || info["expires_at"] == "" {
		t.Errorf("info = %v", info)
	}

	for _, bad := range []map[string]any{
		{"file_id": id, "management_token": "wrong"},
		{"file_id": "nope+mcp", "management_token": token},
		{"file_id": "../x", "management_token": token},
	} {
		if _, errText := m.call("delete_file", bad); errText != "invalid file_id or management_token" {
			t.Errorf("delete_file(%v) error = %q", bad, errText)
		}
	}

	if _, errText := m.call("delete_file", map[string]any{"file_id": id, "management_token": token}); errText != "" {
		t.Fatal(errText)
	}
	expectStatus(t, e.get(download), http.StatusNotFound)
}

func TestMCPUploadValidation(t *testing.T) {
	e := newEnv(t)
	m := e.mcp()
	content := base64.StdEncoding.EncodeToString([]byte("x"))
	cases := []map[string]any{
		{"content": content},
		{"filename": "a"},
		{"filename": "a", "content": "!!!"},
		{"filename": "a", "content": content, "expires_in": "1y"},
		{"filename": "a", "content": content, "disposition": "bogus"},
	}
	for _, args := range cases {
		if _, errText := m.call("upload_file", args); errText == "" {
			t.Errorf("upload_file(%v) succeeded", args)
		}
	}
	if keys := e.bucketKeys(); len(keys) != 0 {
		t.Errorf("rejected uploads left objects: %v", keys)
	}
}

// Uploads made by earlier versions stored the MCP token in plaintext.
func TestLegacyPlaintextToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	info, _ := json.Marshal(handler.FileInfo{
		ID: "legacy+mcp", Size: 1, Offset: 1,
		MetaData: handler.MetaData{files.MetaLegacyToken: "oldtoken", "filetype": "text/plain"},
	})
	for key, body := range map[string]string{"uploads/legacy": "x", "uploads/legacy.info": string(info)} {
		if _, err := e.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(testutil.Bucket), Key: aws.String(key), Body: strings.NewReader(body),
		}); err != nil {
			t.Fatal(err)
		}
	}

	expectStatus(t, e.get(e.srv.URL+"/files/legacy+mcp"), http.StatusOK)

	// HEAD must not leak the plaintext token.
	req, _ := http.NewRequest(http.MethodHead, e.srv.URL+"/files/legacy+mcp", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	if md := e.do(req).Header.Get("Upload-Metadata"); strings.Contains(md, base64.StdEncoding.EncodeToString([]byte("oldtoken"))) {
		t.Errorf("HEAD leaks legacy token: %q", md)
	}

	req, _ = http.NewRequest(http.MethodDelete, e.srv.URL+"/api/files/legacy+mcp", nil)
	req.Header.Set("Authorization", "Bearer oldtoken")
	expectStatus(t, e.do(req), http.StatusNoContent)
}

func TestShareURL(t *testing.T) {
	e := newEnv(t)
	u := e.upload("hello", map[string]string{"filename": "бележка 1.txt", "filetype": "text/plain"})

	resp := e.create(1, map[string]string{"filename": "a/b.txt"})
	if got := resp.Header.Get(hooks.HeaderShareURL); !strings.HasSuffix(got, "/a_b.txt") {
		t.Errorf("slash in filename not sanitised: %q", got)
	}

	share := "https://share.test/files/" + u.objectID + "/" + url.PathEscape("бележка 1.txt")
	var info map[string]any
	json.NewDecoder(e.get(e.srv.URL+"/api/files/"+u.id, "Authorization", "Bearer "+u.token).Body).Decode(&info) //nolint:errcheck
	if info["download_url"] != share {
		t.Errorf("download_url = %v, want %v", info["download_url"], share)
	}

	local := strings.Replace(share, "https://share.test", e.srv.URL, 1)
	resp = e.get(local)
	expectStatus(t, resp, http.StatusOK)
	if body := readBody(resp); body != "hello" {
		t.Errorf("body = %q", body)
	}
	// Any name works; only the ID matters.
	expectStatus(t, e.get(e.srv.URL+"/files/"+u.objectID+"/other.txt"), http.StatusOK)

	expectStatus(t, e.get(e.srv.URL+"/files/"+u.objectID+"/a/b"), http.StatusNotFound)
	expectStatus(t, e.get(e.srv.URL+"/files/"+u.objectID+"/"), http.StatusNotFound)
	req, _ := http.NewRequest(http.MethodDelete, local, nil)
	req.Header.Set("Authorization", "Bearer "+u.token)
	expectStatus(t, e.do(req), http.StatusNotFound)
}

func TestPasswordUnlockOnShareURL(t *testing.T) {
	e := newEnv(t)
	u := e.upload("secret", map[string]string{"filename": "s.txt", "filetype": "text/plain", "password": "pw"})
	path := "/files/" + u.objectID + "/s.txt"

	resp := e.get(e.srv.URL+path, "Accept", "text/html")
	expectStatus(t, resp, http.StatusUnauthorized)
	if body := readBody(resp); !strings.Contains(body, `action="`+path+`"`) {
		t.Errorf("form does not post back to the share URL: %s", body)
	}

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: e.client.CheckRedirect}
	resp, err := browser.PostForm(e.srv.URL+path, url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusSeeOther)
	if loc := resp.Header.Get("Location"); loc != path {
		t.Errorf("redirect = %q, want %q", loc, path)
	}
	// The cookie is per object, so it also unlocks the tus URL.
	for _, p := range []string{path, "/files/" + u.id} {
		resp, err := browser.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, resp, http.StatusOK)
		resp.Body.Close()
	}
}

func TestBrowserErrorPages(t *testing.T) {
	e := newEnv(t)
	pending := e.start(10, map[string]string{"filename": "later.txt"})

	cases := []struct {
		url    string
		status int
		text   string
	}{
		{e.srv.URL + "/files/doesnotexist", http.StatusNotFound, "File not available"},
		{e.srv.URL + "/files/doesnotexist/name.pdf", http.StatusNotFound, "File not available"},
		{pending.location, http.StatusConflict, "Upload still in progress"},
	}
	for _, c := range cases {
		resp := e.get(c.url, "Accept", "text/html,application/xhtml+xml")
		expectStatus(t, resp, c.status)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q", c.url, ct)
		}
		if body := readBody(resp); !strings.Contains(body, c.text) || !strings.Contains(body, `href="/"`) {
			t.Errorf("%s: page missing %q", c.url, c.text)
		}

		resp = e.get(c.url)
		expectStatus(t, resp, c.status)
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: API Content-Type = %q", c.url, ct)
		}
	}
}

func TestMediaIsNotSandboxed(t *testing.T) {
	e := newEnv(t)
	for filetype, sandboxed := range map[string]bool{
		"video/mp4":     false,
		"audio/mpeg":    false,
		"image/png":     false,
		"image/svg+xml": true,
		"text/html":     true,
		"text/plain":    true,
	} {
		u := e.upload("x", map[string]string{"filetype": filetype})
		csp := e.get(u.location).Header.Get("Content-Security-Policy")
		if got := strings.Contains(csp, "sandbox"); got != sandboxed {
			t.Errorf("%s: sandboxed = %v, want %v (%q)", filetype, got, sandboxed, csp)
		}
		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP missing default-src 'none': %q", filetype, csp)
		}
	}
}
