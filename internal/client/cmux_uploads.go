package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
	"golang.org/x/sys/unix"
)

const cmuxUploadTTL = 5 * time.Minute

type cmuxUpload struct {
	ownerID, kind, workspaceID, surfaceID      string
	root, directory, name, tempName, finalName string
	size, written                              int64
	expiresAt                                  time.Time
	dir, file                                  *os.File
}

func (upload *cmuxUpload) cleanup() {
	if upload.file != nil {
		_ = upload.file.Close()
		upload.file = nil
	}
	if upload.dir != nil {
		_ = unix.Unlinkat(int(upload.dir.Fd()), upload.tempName, 0)
		_ = upload.dir.Close()
		upload.dir = nil
	}
}

func (m *CmuxManager) executeUploadRequest(request protocol.CmuxRequestPayload) (json.RawMessage, error) {
	if request.ViewerID == "" || len(request.ViewerID) > 128 {
		return nil, errors.New("上传请求缺少可信 Web 连接身份")
	}
	m.uploadMu.Lock()
	defer m.uploadMu.Unlock()
	now := time.Now()
	m.pruneUploadsLocked(now)
	if request.Action == protocol.CmuxUploadBegin {
		if len(m.uploads) >= 16 {
			return nil, errors.New("同时上传的文件过多")
		}
		return m.beginUpload(request, now)
	}
	upload := m.uploads[request.UploadID]
	if upload == nil || upload.ownerID != request.ViewerID ||
		upload.kind != request.UploadKind ||
		upload.workspaceID != request.WorkspaceID || upload.surfaceID != request.SurfaceID {
		return nil, errors.New("上传会话无效或已过期")
	}
	upload.expiresAt = now.Add(cmuxUploadTTL)
	switch request.Action {
	case protocol.CmuxUploadChunk:
		if request.Offset != upload.written ||
			int64(len(request.Data)) > upload.size-upload.written {
			return nil, errors.New("上传分片顺序或大小不正确")
		}
		count, err := upload.file.Write(request.Data)
		if err != nil || count != len(request.Data) {
			upload.cleanup()
			delete(m.uploads, request.UploadID)
			return nil, errors.New("写入上传文件失败")
		}
		upload.written += int64(count)
		return protocol.MustMarshal(struct {
			Written int64 `json:"written"`
		}{upload.written}), nil
	case protocol.CmuxUploadAbort:
		upload.cleanup()
		delete(m.uploads, request.UploadID)
		return protocol.MustMarshal(struct {
			OK bool `json:"ok"`
		}{true}), nil
	case protocol.CmuxUploadFinish:
		if upload.written != upload.size {
			return nil, errors.New("文件尚未上传完整")
		}
		delete(m.uploads, request.UploadID)
		defer upload.cleanup()
		if err := upload.file.Close(); err != nil {
			upload.file = nil
			return nil, errors.New("保存上传文件失败")
		}
		upload.file = nil
		// 防止上传期间开放目录被移动/替换后将文件写到根目录外。
		if upload.kind == "file" {
			root, relative, err := matchCmuxFileRoot(m.fileRoots(), upload.directory)
			if err != nil {
				return nil, errors.New("上传目录不再开放")
			}
			currentDir, err := openCmuxDirectory(root, relative)
			if err != nil {
				return nil, errors.New("上传目录已变更")
			}
			var opened, current unix.Stat_t
			statError := unix.Fstat(int(upload.dir.Fd()), &opened)
			if statError == nil {
				statError = unix.Fstat(int(currentDir.Fd()), &current)
			}
			_ = currentDir.Close()
			if statError != nil || opened.Dev != current.Dev || opened.Ino != current.Ino {
				return nil, errors.New("上传目录已变更")
			}
		}
		if err := unix.Linkat(int(upload.dir.Fd()), upload.tempName,
			int(upload.dir.Fd()), upload.finalName, 0); err != nil {
			return nil, errors.New("同名文件已存在或保存上传文件失败")
		}
		return protocol.MustMarshal(struct {
			Path string `json:"path"`
			Name string `json:"name"`
			Size int64  `json:"size"`
		}{filepath.Join(upload.directory, upload.finalName), upload.name, upload.size}), nil
	default:
		return nil, errors.New("不支持的上传操作")
	}
}

func (m *CmuxManager) beginUpload(request protocol.CmuxRequestPayload, now time.Time) (json.RawMessage, error) {
	var root, directory string
	var dir *os.File
	var err error
	if request.UploadKind == "file" {
		roots := m.fileRoots()
		root, _, err = matchCmuxFileRoot(roots, request.Path)
		if err != nil {
			return nil, err
		}
		directory = filepath.Clean(request.Path)
		if m.uploadDirectoryShared(directory) {
			return nil, errors.New("不能向 HTML 分享目录上传文件")
		}
		relative, _ := filepath.Rel(root, directory)
		dir, err = openCmuxDirectory(root, relative)
	} else {
		directory, err = privateCmuxUploadDir()
		if err == nil {
			if m.uploadDirectoryShared(directory) {
				return nil, errors.New("附件目录与 HTML 分享目录重叠")
			}
			dir, err = openCmuxDirectory(directory, ".")
		}
	}
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	finalName := request.FileName
	if request.UploadKind == "attachment" {
		finalName = id + safeAttachmentExtension(request.FileName)
	}
	tempName := ".cf-upload-" + id
	fd, err := unix.Openat(int(dir.Fd()), tempName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		_ = dir.Close()
		return nil, errors.New("无法创建上传临时文件")
	}
	upload := &cmuxUpload{
		ownerID: request.ViewerID, kind: request.UploadKind,
		workspaceID: request.WorkspaceID, surfaceID: request.SurfaceID,
		root: root, directory: directory, name: request.FileName,
		tempName: tempName, finalName: finalName,
		size: request.Size, expiresAt: now.Add(cmuxUploadTTL),
		dir: dir, file: os.NewFile(uintptr(fd), tempName),
	}
	m.uploads[id] = upload
	return protocol.MustMarshal(struct {
		UploadID string `json:"upload_id"`
	}{id}), nil
}

func (m *CmuxManager) uploadDirectoryShared(directory string) bool {
	if m.config.ShareRoot == "" {
		return false
	}
	shared, err := expandAbsPath(m.config.ShareRoot)
	if err != nil {
		return true
	}
	shared, err = filepath.EvalSymlinks(shared)
	if err != nil {
		return true
	}
	relative, err := filepath.Rel(shared, directory)
	return err != nil || relative == "." || relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func privateCmuxUploadDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("无法确定附件保存目录")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return "", errors.New("无法确定附件保存目录")
	}
	parent := filepath.Join(home, ".claude-forward")
	target := filepath.Join(parent, "cmux-uploads")
	for _, directory := range []string{parent, target} {
		if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
			return "", errors.New("无法创建附件保存目录")
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("附件保存目录不可用")
		}
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("附件保存目录权限过宽")
	}
	return target, nil
}

func safeAttachmentExtension(name string) string {
	extension := filepath.Ext(name)
	if len(extension) < 2 || len(extension) > 13 {
		return ""
	}
	for _, character := range strings.TrimPrefix(extension, ".") {
		if character > unicode.MaxASCII || !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return ""
		}
	}
	return extension
}

// 客户端退出或重连时关闭未完成的上传，避免保留半文件。
func (m *CmuxManager) closeUploads() {
	m.uploadMu.Lock()
	defer m.uploadMu.Unlock()
	for id, upload := range m.uploads {
		upload.cleanup()
		delete(m.uploads, id)
	}
}

func (m *CmuxManager) pruneUploadsLocked(now time.Time) {
	for id, upload := range m.uploads {
		if !now.Before(upload.expiresAt) {
			upload.cleanup()
			delete(m.uploads, id)
		}
	}
}

func (m *CmuxManager) pruneUploadsUntilDone(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.uploadMu.Lock()
			m.pruneUploadsLocked(now)
			m.uploadMu.Unlock()
		}
	}
}
