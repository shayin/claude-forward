package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func newTestHTMLShare(t *testing.T) (*HTMLShare, *Config, string) {
	t.Helper()
	root := t.TempDir()
	config := DefaultConfig()
	config.Path = t.TempDir()
	config.Client.ID = "client-a"
	config.Server.URL = "wss://example.test:6022/ws"
	config.HTMLShare.RootDir = root
	share, err := newHTMLShare(config)
	if err != nil {
		t.Fatalf("newHTMLShare: %v", err)
	}
	return share, config, root
}

func TestHTMLShareReadEnforcesRootAndToken(t *testing.T) {
	share, _, root := newTestHTMLShare(t)
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>ok</h1>"), 0600); err != nil {
		t.Fatal(err)
	}
	got := share.read(protocol.FileRequestPayload{RequestID: "1", Token: share.token, Path: "index.html"})
	if got.StatusCode != 200 || string(got.Content) != "<h1>ok</h1>" || !strings.HasPrefix(got.ContentType, "text/html") {
		t.Fatalf("unexpected response: %#v", got)
	}
	if got := share.read(protocol.FileRequestPayload{Token: "wrong", Path: "index.html"}); got.StatusCode != 403 {
		t.Fatalf("wrong token status = %d", got.StatusCode)
	}
	if got := share.read(protocol.FileRequestPayload{Token: share.token, Path: "../secret"}); got.StatusCode != 404 {
		t.Fatalf("traversal status = %d", got.StatusCode)
	}
}

func TestHTMLShareReadRejectsSymlinkEscapeAndOversizeFile(t *testing.T) {
	share, _, root := newTestHTMLShare(t)
	outside := filepath.Join(t.TempDir(), "secret.html")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.html")); err != nil {
		t.Fatal(err)
	}
	if got := share.read(protocol.FileRequestPayload{Token: share.token, Path: "escape.html"}); got.StatusCode != 404 {
		t.Fatalf("symlink escape status = %d", got.StatusCode)
	}
	large := filepath.Join(root, "large.html")
	if err := os.WriteFile(large, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, maxSharedFileSize+1); err != nil {
		t.Fatal(err)
	}
	if got := share.read(protocol.FileRequestPayload{Token: share.token, Path: "large.html"}); got.StatusCode != 413 {
		t.Fatalf("oversize status = %d", got.StatusCode)
	}
}

func TestHTMLShareSnapshotAndLinks(t *testing.T) {
	share, config, root := newTestHTMLShare(t)
	before := share.snapshot()
	for _, name := range []string{"b.htm", "nested/a.html", "style.css"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Ensure a changed existing HTML is also detected.
	old := filepath.Join(root, "old.html")
	if err := os.WriteFile(old, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	before = share.snapshot()
	if err := os.WriteFile(old, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	links := share.linksText(config.Client.ID, before)
	if strings.Count(links, "HTML 预览链接：") != 1 || !strings.Contains(links, "/share/client-a/") || !strings.Contains(links, "old.html") {
		t.Fatalf("links = %q", links)
	}
	if got := derivePublicBaseURL("ws://host.test:80/ws", ""); got != "http://host.test:80" {
		t.Fatalf("base URL = %q", got)
	}
}
