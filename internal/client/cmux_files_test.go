package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func TestCmuxFilesStayWithinExplicitRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	outside := filepath.Join(base, "private")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root) // macOS 上 /var 会规范化为 /private/var。
	if err := os.WriteFile(filepath.Join(root, "src", "hello.txt"), []byte("hello world"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "linked-file")); err != nil {
		t.Fatal(err)
	}
	manager := NewCmuxManager(CmuxConfig{Enabled: true, FileRoots: []string{root, root, "relative"}})
	execute := func(request protocol.CmuxRequestPayload) json.RawMessage {
		t.Helper()
		request.RequestID = "files"
		raw, err := manager.Execute(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var roots struct {
		Roots []string `json:"roots"`
	}
	if err := json.Unmarshal(execute(protocol.CmuxRequestPayload{Action: protocol.CmuxFileRoots}), &roots); err != nil ||
		len(roots.Roots) != 1 || roots.Roots[0] != root {
		t.Fatalf("unexpected roots: %+v err=%v", roots, err)
	}
	var listing struct {
		Path    string          `json:"path"`
		Parent  string          `json:"parent"`
		Entries []cmuxFileEntry `json:"entries"`
	}
	if err := json.Unmarshal(execute(protocol.CmuxRequestPayload{Action: protocol.CmuxFileList}), &listing); err != nil ||
		listing.Path != root || listing.Parent != "" || len(listing.Entries) != 3 {
		t.Fatalf("unexpected root listing: %+v err=%v", listing, err)
	}
	if listing.Entries[0].Name != "src" || listing.Entries[0].Type != "directory" {
		t.Fatalf("directory order/type incorrect: %+v", listing.Entries)
	}
	if err := json.Unmarshal(execute(protocol.CmuxRequestPayload{
		Action: protocol.CmuxFileList, Path: filepath.Join(root, "src"), Query: "HELLO",
	}), &listing); err != nil || listing.Parent != root || len(listing.Entries) != 1 {
		t.Fatalf("query/parent incorrect: %+v err=%v", listing, err)
	}
	var content struct {
		Data   []byte `json:"data"`
		Size   int64  `json:"size"`
		Offset int64  `json:"offset"`
	}
	if err := json.Unmarshal(execute(protocol.CmuxRequestPayload{
		Action: protocol.CmuxFileRead, Path: filepath.Join(root, "src", "hello.txt"),
		Offset: 6, ReadBytes: 5,
	}), &content); err != nil || content.Size != 11 || content.Offset != 6 ||
		string(content.Data) != "world" {
		t.Fatalf("file chunk incorrect: %+v err=%v", content, err)
	}
	for _, path := range []string{
		filepath.Join(root, "..", "private", "secret.txt"),
		filepath.Join(root, "linked-dir", "secret.txt"),
		filepath.Join(root, "linked-file"),
	} {
		_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
			RequestID: "forbidden", Action: protocol.CmuxFileRead, Path: path, ReadBytes: 8,
		})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe path accepted or leaked information: %s %v", path, err)
		}
	}
}

func TestCmuxFileListDisabledWithoutRoots(t *testing.T) {
	manager := NewCmuxManager(CmuxConfig{Enabled: true})
	raw, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "roots", Action: protocol.CmuxFileRoots,
	})
	if err != nil || string(raw) != `{"roots":[]}` {
		t.Fatalf("empty roots: %s %v", raw, err)
	}
	if _, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "list", Action: protocol.CmuxFileList,
	}); err == nil {
		t.Fatal("unconfigured file manager was enabled")
	}
}

func TestCmuxRecentFilesSkipSymlinksAndExcludedDirectories(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	outside := filepath.Join(base, "private")
	for _, path := range []string{root, outside, filepath.Join(root, "src"),
		filepath.Join(root, "node_modules")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	root, _ = filepath.EvalSymlinks(root)
	for path, content := range map[string]string{
		filepath.Join(root, "src", "new.md"):         "new",
		filepath.Join(root, "old.html"):              "old",
		filepath.Join(root, "node_modules", "x.txt"): "excluded",
		filepath.Join(outside, "secret.md"):          "secret",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "old.html"), older, older); err != nil {
		t.Fatal(err)
	}
	manager := NewCmuxManager(CmuxConfig{Enabled: true, FileRoots: []string{root}})
	raw, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "recent", Action: protocol.CmuxFileRecent,
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Files []cmuxFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Files) != 2 ||
		result.Files[0].Name != "new.md" || result.Files[1].Name != "old.html" {
		t.Fatalf("recent file scan escaped roots or lost order: %+v %v", result.Files, err)
	}
}
