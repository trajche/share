package hooks

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
	"sharemk/internal/files"
	"sharemk/internal/testutil"
)

func newHooks() *Hooks {
	cfg := &config.Config{PublicURL: "https://share.test/", TUSBasePath: "/files/", S3ObjectPrefix: "uploads/"}
	return New(cfg, files.NewService(cfg, nil))
}

func preCreate(t *testing.T, meta handler.MetaData) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	t.Helper()
	return newHooks().PreCreate(handler.HookEvent{Upload: handler.FileInfo{MetaData: meta}})
}

func TestPreCreateDefaultsAndToken(t *testing.T) {
	resp, changes, err := preCreate(t, handler.MetaData{"filename": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}

	if changes.MetaData[files.MetaExpiresIn] != files.DefaultExpiry {
		t.Errorf("expires-in = %q, want default", changes.MetaData[files.MetaExpiresIn])
	}
	if changes.MetaData["filename"] != "a.txt" {
		t.Error("client metadata dropped")
	}

	token := resp.Header[HeaderManagementToken]
	if len(token) != 64 {
		t.Fatalf("management token header = %q", token)
	}
	if !files.TokenMatches(changes.MetaData, token) {
		t.Error("stored token hash does not match the returned token")
	}
	if strings.Contains(strings.Join(mapValues(changes.MetaData), " "), token) {
		t.Error("plaintext token stored in metadata")
	}

	if _, _, ok := files.SplitID(changes.ID); !ok || len(changes.ID) != 32 {
		t.Errorf("preset ID = %q", changes.ID)
	}
	wantURL := "https://share.test/manage/" + changes.ID + "#" + token
	if got := resp.Header[HeaderManageURL]; got != wantURL {
		t.Errorf("manage URL = %q, want %q", got, wantURL)
	}
	if got, want := resp.Header[HeaderShareURL], "https://share.test/files/"+changes.ID+"/a.txt"; got != want {
		t.Errorf("share URL = %q, want %q", got, want)
	}
	if resp.StatusCode != 0 {
		t.Errorf("status overridden to %d", resp.StatusCode)
	}
}

func TestPreCreateUniqueTokens(t *testing.T) {
	r1, c1, _ := preCreate(t, nil)
	r2, c2, _ := preCreate(t, nil)
	if r1.Header[HeaderManagementToken] == r2.Header[HeaderManagementToken] || c1.ID == c2.ID {
		t.Error("tokens or IDs repeat")
	}
}

func TestPreCreateStripsServerOwnedKeys(t *testing.T) {
	_, changes, err := preCreate(t, handler.MetaData{
		files.MetaTokenHash:    files.HashToken("attacker"),
		files.MetaLegacyToken:  "attacker",
		files.MetaPasswordHash: "pbkdf2-sha256$1$AAAA$AAAA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if files.TokenMatches(changes.MetaData, "attacker") {
		t.Error("client-chosen token accepted")
	}
	if _, ok := changes.MetaData[files.MetaLegacyToken]; ok {
		t.Error("client legacy token kept")
	}
	if _, ok := changes.MetaData[files.MetaPasswordHash]; ok {
		t.Error("client password hash kept")
	}
}

func TestPreCreatePassword(t *testing.T) {
	_, changes, err := preCreate(t, handler.MetaData{files.MetaPassword: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changes.MetaData[files.MetaPassword]; ok {
		t.Fatal("plaintext password stored")
	}
	if !files.VerifyPassword(changes.MetaData[files.MetaPasswordHash], "s3cret") {
		t.Error("stored hash does not verify")
	}

	// An empty password means no protection.
	_, changes, err = preCreate(t, handler.MetaData{files.MetaPassword: ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changes.MetaData[files.MetaPasswordHash]; ok {
		t.Error("empty password produced a hash")
	}
}

func TestPreCreateRejects(t *testing.T) {
	cases := map[string]handler.MetaData{
		"ERR_INVALID_EXPIRES_IN":  {files.MetaExpiresIn: "1y"},
		"ERR_INVALID_DISPOSITION": {files.MetaDisposition: "download"},
		"ERR_INVALID_PASSWORD":    {files.MetaPassword: strings.Repeat("x", files.MaxPasswordLength+1)},
	}
	for code, meta := range cases {
		_, _, err := preCreate(t, meta)
		var herr handler.Error
		if !errors.As(err, &herr) {
			t.Errorf("%s: err = %v, want handler.Error", code, err)
			continue
		}
		if herr.ErrorCode != code || herr.HTTPResponse.StatusCode != http.StatusBadRequest {
			t.Errorf("got %s/%d, want %s/400", herr.ErrorCode, herr.HTTPResponse.StatusCode, code)
		}
	}
}

func TestHandleCompleteTagsExpiry(t *testing.T) {
	client, cfg := testutil.NewS3(t)
	h := New(cfg, files.NewService(cfg, client))
	ctx := context.Background()

	for _, k := range []string{"uploads/obj", "uploads/obj.info"} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(testutil.Bucket), Key: aws.String(k), Body: strings.NewReader("x"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	before := time.Now().UTC()
	h.HandleComplete(handler.HookEvent{Upload: handler.FileInfo{
		ID:       "obj+1",
		MetaData: handler.MetaData{files.MetaExpiresIn: "7d"},
		Storage:  map[string]string{"Key": "uploads/obj"},
	}})

	for _, k := range []string{"uploads/obj", "uploads/obj.info"} {
		out, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(testutil.Bucket), Key: aws.String(k)})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.TagSet) != 1 || aws.ToString(out.TagSet[0].Key) != "expires-at" {
			t.Fatalf("%s tags = %+v", k, out.TagSet)
		}
		at, err := time.Parse(time.RFC3339, aws.ToString(out.TagSet[0].Value))
		if err != nil {
			t.Fatal(err)
		}
		if d := at.Sub(before); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
			t.Errorf("%s expires in %v, want ~7d", k, d)
		}
	}
}

func mapValues(m handler.MetaData) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
