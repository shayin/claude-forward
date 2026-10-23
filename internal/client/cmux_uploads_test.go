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

func TestCmuxChunkedFileUploadIsExclusiveAndBoundToViewer(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	manager := NewCmuxManager(CmuxConfig{Enabled: true, FileRoots: []string{root}})
	execute := func(request protocol.CmuxRequestPayload) json.RawMessage {
		t.Helper()
		request.RequestID = "upload"
		raw, err := manager.Execute(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	begin := protocol.CmuxRequestPayload{
		Action: protocol.CmuxUploadBegin, UploadKind: "file", ViewerID: "owner",
		Path: root, FileName: "报告.txt", Size: 5,
	}
	var started struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(execute(begin), &started); err != nil || started.UploadID == "" {
		t.Fatalf("upload id missing: %+v %v", started, err)
	}
	chunk := protocol.CmuxRequestPayload{
		Action: protocol.CmuxUploadChunk, UploadKind: "file", ViewerID: "owner",
		UploadID: started.UploadID, Data: []byte("hello"),
	}
	if _, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "wrong", Action: protocol.CmuxUploadChunk, UploadKind: "file",
		ViewerID: "other", UploadID: started.UploadID, Data: []byte("bad"),
	}); err == nil {
		t.Fatal("another viewer appended to upload")
	}
	execute(chunk)
	if _, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "duplicate", Action: protocol.CmuxUploadChunk, UploadKind: "file",
		ViewerID: "owner", UploadID: started.UploadID, Data: []byte("x"),
	}); err == nil {
		t.Fatal("duplicate chunk was accepted")
	}
	var finished struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(execute(protocol.CmuxRequestPayload{
		Action: protocol.CmuxUploadFinish, UploadKind: "file",
		ViewerID: "owner", UploadID: started.UploadID,
	}), &finished); err != nil || finished.Path != filepath.Join(root, "报告.txt") ||
		finished.Size != 5 {
		t.Fatalf("wrong completion: %+v %v", finished, err)
	}
	data, err := os.ReadFile(finished.Path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("uploaded bytes wrong: %s %v", data, err)
	}
	info, err := os.Stat(finished.Path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("upload permissions too broad: %+v %v", info, err)
	}
	started = struct {
		UploadID string `json:"upload_id"`
	}{}
	if err := json.Unmarshal(execute(begin), &started); err != nil {
		t.Fatal(err)
	}
	chunk.UploadID = started.UploadID
	execute(chunk)
	if _, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "existing", Action: protocol.CmuxUploadFinish, UploadKind: "file",
		ViewerID: "owner", UploadID: started.UploadID,
	}); err == nil {
		t.Fatal("existing file was overwritten")
	}
	data, _ = os.ReadFile(finished.Path)
	if string(data) != "hello" {
		t.Fatal("existing file content changed")
	}
}

func TestCmuxAttachmentUploadBindsSurfaceAndUsesPrivateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	home, _ = filepath.EvalSymlinks(home)
	manager := NewCmuxManager(CmuxConfig{Enabled: true})
	begin := protocol.CmuxRequestPayload{
		RequestID: "begin", Action: protocol.CmuxUploadBegin, UploadKind: "attachment",
		ViewerID: "owner", WorkspaceID: "w", SurfaceID: "s", FileName: "image.png", Size: 3,
	}
	raw, err := manager.Execute(context.Background(), begin)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		UploadID string `json:"upload_id"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	chunk := protocol.CmuxRequestPayload{
		RequestID: "chunk", Action: protocol.CmuxUploadChunk, UploadKind: "attachment",
		ViewerID: "owner", WorkspaceID: "w", SurfaceID: "s", UploadID: result.UploadID,
		Data: []byte{1, 2, 3},
	}
	wrong := chunk
	wrong.SurfaceID = "another"
	if _, err := manager.Execute(context.Background(), wrong); err == nil {
		t.Fatal("attachment was sent to another surface")
	}
	if _, err := manager.Execute(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	chunk.Action, chunk.RequestID, chunk.Data = protocol.CmuxUploadFinish, "finish", nil
	raw, err = manager.Execute(context.Background(), chunk)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil ||
		!strings.HasPrefix(result.Path, filepath.Join(home, ".claude-forward", "cmux-uploads")+string(filepath.Separator)) ||
		filepath.Ext(result.Path) != ".png" {
		t.Fatalf("attachment path outside private root: %+v %v", result, err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil || len(data) != 3 {
		t.Fatalf("attachment content wrong: %v %v", data, err)
	}
	// 未完成的上传在断线时应清除，不保留用户半文件。
	raw, err = manager.Execute(context.Background(), begin)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	manager.closeUploads()
	if len(manager.uploads) != 0 {
		t.Fatal("unfinished uploads remained after disconnect")
	}
}

func TestCmuxUploadsCannotWriteIntoHTMLShareRoot(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	manager := NewCmuxManager(CmuxConfig{
		Enabled: true, FileRoots: []string{root}, ShareRoot: shared,
	})
	_, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "shared", Action: protocol.CmuxUploadBegin, UploadKind: "file",
		ViewerID: "owner", Path: shared, FileName: "a.txt", Size: 2,
	})
	if err == nil {
		t.Fatal("file upload wrote into HTML share directory")
	}
	if _, err := os.Stat(filepath.Join(shared, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("shared file was created: %v", err)
	}
}

func TestCmuxUploadRejectsMovedDirectoryEvenIfReplacedWithSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	subdir := filepath.Join(root, "sub")
	outside := filepath.Join(base, "moved-out")
	if err := os.MkdirAll(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	subdir = filepath.Join(root, "sub")
	manager := NewCmuxManager(CmuxConfig{Enabled: true, FileRoots: []string{root}})
	raw, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "begin", Action: protocol.CmuxUploadBegin, UploadKind: "file",
		ViewerID: "owner", Path: subdir, FileName: "final.txt", Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		ID string `json:"upload_id"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(subdir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, subdir); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "chunk", Action: protocol.CmuxUploadChunk, UploadKind: "file",
		ViewerID: "owner", UploadID: started.ID, Data: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "finish", Action: protocol.CmuxUploadFinish, UploadKind: "file",
		ViewerID: "owner", UploadID: started.ID,
	}); err == nil {
		t.Fatal("upload escaped configured root through symlink replacement")
	}
	if _, err := os.Stat(filepath.Join(outside, "final.txt")); !os.IsNotExist(err) {
		t.Fatalf("file created outside root: %v", err)
	}
}

func TestCmuxExpiredUploadIsPrunedWithoutAnotherRequest(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	manager := NewCmuxManager(CmuxConfig{Enabled: true, FileRoots: []string{root}})
	raw, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{
		RequestID: "begin", Action: protocol.CmuxUploadBegin, UploadKind: "file",
		ViewerID: "owner", Path: root, FileName: "final.txt", Size: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		ID string `json:"upload_id"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	manager.uploadMu.Lock()
	upload := manager.uploads[started.ID]
	temporary := filepath.Join(root, upload.tempName)
	upload.expiresAt = time.Now().Add(-time.Second)
	manager.pruneUploadsLocked(time.Now())
	_, exists := manager.uploads[started.ID]
	manager.uploadMu.Unlock()
	if exists {
		t.Fatal("expired upload remained in active map")
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("expired half-file remained: %v", err)
	}
}
