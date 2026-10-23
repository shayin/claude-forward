package client

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
	"golang.org/x/sys/unix"
)

const (
	cmuxFileListLimit = 200
	cmuxFileScanLimit = 5000
)

type cmuxFileEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
}

func (m *CmuxManager) executeFileRequest(request protocol.CmuxRequestPayload) (json.RawMessage, error) {
	roots := m.fileRoots()
	if request.Action == protocol.CmuxFileRoots {
		return protocol.MustMarshal(struct {
			Roots []string `json:"roots"`
		}{roots}), nil
	}
	if len(roots) == 0 {
		return nil, errors.New("未配置 cmux.file_roots；文件管理保持关闭")
	}
	target := request.Path
	if target == "" {
		target = roots[0]
	}
	root, relative, err := matchCmuxFileRoot(roots, target)
	if err != nil {
		return nil, err
	}
	switch request.Action {
	case protocol.CmuxFileList:
		return listCmuxFiles(root, relative, request)
	case protocol.CmuxFileRead:
		return readCmuxFile(root, relative, request)
	case protocol.CmuxFileRecent:
		return recentCmuxFiles(root, relative)
	}
	return nil, errors.New("不支持的文件请求")
}

func (m *CmuxManager) fileRoots() []string {
	roots := make([]string, 0, len(m.config.FileRoots))
	for _, configured := range m.config.FileRoots {
		if strings.HasPrefix(configured, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			configured = filepath.Join(home, configured[2:])
		}
		if !filepath.IsAbs(configured) {
			continue
		}
		real, err := filepath.EvalSymlinks(configured)
		if err != nil {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			continue
		}
		found := false
		for _, root := range roots {
			if root == real {
				found = true
				break
			}
		}
		if !found {
			roots = append(roots, real)
		}
	}
	return roots
}

func matchCmuxFileRoot(roots []string, target string) (root, relative string, err error) {
	if !filepath.IsAbs(target) {
		return "", "", errors.New("文件路径必须为绝对路径")
	}
	clean := filepath.Clean(target)
	for _, candidate := range roots {
		rel, relativeError := filepath.Rel(candidate, clean)
		if relativeError != nil || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if len(candidate) > len(root) {
			root, relative = candidate, rel
		}
	}
	if root == "" {
		return "", "", errors.New("文件路径不在 cmux.file_roots 范围内")
	}
	return root, relative, nil
}

// 逐级使用 O_NOFOLLOW 打开目录，不允许工作区中的软链接跳出配置的根。
func openCmuxDirectory(root, relative string) (*os.File, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("文件目录不可访问")
	}
	if relative != "." {
		for _, name := range strings.Split(relative, string(filepath.Separator)) {
			if name == "." || name == ".." || name == "" {
				_ = unix.Close(fd)
				return nil, errors.New("目录路径无效")
			}
			next, openError := unix.Openat(fd, name,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			_ = unix.Close(fd)
			if openError != nil {
				return nil, errors.New("目录不可访问或包含符号链接")
			}
			fd = next
		}
	}
	return os.NewFile(uintptr(fd), filepath.Join(root, relative)), nil
}

func listCmuxFiles(root, relative string, request protocol.CmuxRequestPayload) (json.RawMessage, error) {
	directory, err := openCmuxDirectory(root, relative)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries := make([]cmuxFileEntry, 0, cmuxFileListLimit)
	query := strings.ToLower(request.Query)
	inspected := 0
	for inspected < cmuxFileScanLimit {
		batch, readError := directory.ReadDir(min(256, cmuxFileScanLimit-inspected))
		for _, item := range batch {
			inspected++
			if query != "" && !strings.Contains(strings.ToLower(item.Name()), query) {
				continue
			}
			var stat unix.Stat_t
			if unix.Fstatat(int(directory.Fd()), item.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW) != nil {
				continue
			}
			kind := "other"
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				kind = "directory"
			case unix.S_IFREG:
				kind = "file"
			}
			entries = append(entries, cmuxFileEntry{
				Name: item.Name(), Path: filepath.Join(root, relative, item.Name()),
				Type: kind, Size: stat.Size, ModifiedAt: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC().Format(time.RFC3339Nano),
			})
		}
		if readError == io.EOF || len(batch) == 0 {
			break
		}
		if readError != nil {
			return nil, errors.New("读取文件目录失败")
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if (entries[i].Type == "directory") != (entries[j].Type == "directory") {
			return entries[i].Type == "directory"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	offset := min(int(request.Offset), len(entries))
	end := min(offset+cmuxFileListLimit, len(entries))
	current := filepath.Join(root, relative)
	var parent string
	if current != root {
		parent = filepath.Dir(current)
	}
	return protocol.MustMarshal(struct {
		Path      string          `json:"path"`
		Root      string          `json:"root"`
		Parent    string          `json:"parent,omitempty"`
		Entries   []cmuxFileEntry `json:"entries"`
		Total     int             `json:"total"`
		HasMore   bool            `json:"has_more"`
		Truncated bool            `json:"truncated"`
	}{
		Path: current, Root: root, Parent: parent, Entries: entries[offset:end],
		Total: len(entries), HasMore: end < len(entries), Truncated: inspected >= cmuxFileScanLimit,
	}), nil
}

func readCmuxFile(root, relative string, request protocol.CmuxRequestPayload) (json.RawMessage, error) {
	if relative == "." {
		return nil, errors.New("请选择普通文件")
	}
	directory, err := openCmuxDirectory(root, filepath.Dir(relative))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), filepath.Base(relative),
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("文件不可访问或包含符号链接")
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("只允许读取普通文件")
	}
	data := make([]byte, request.ReadBytes)
	count, err := unix.Pread(fd, data, request.Offset)
	if err != nil {
		return nil, errors.New("读取文件失败")
	}
	return protocol.MustMarshal(struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		Data    []byte `json:"data"` // JSON 自动 base64；每次至多 256 KiB。
		Offset  int64  `json:"offset"`
		Name    string `json:"name"`
		ModTime string `json:"modified_at"`
	}{
		Path: filepath.Join(root, relative), Size: stat.Size, Data: data[:count],
		Offset: request.Offset, Name: filepath.Base(relative),
		ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC().Format(time.RFC3339Nano),
	}), nil
}

func recentCmuxFiles(root, relative string) (json.RawMessage, error) {
	const maxEntries, maxDepth, maxResults = 3000, 5, 60
	allowed := map[string]bool{
		".html": true, ".htm": true, ".md": true, ".markdown": true,
		".pdf": true, ".png": true, ".jpg": true, ".jpeg": true,
		".gif": true, ".webp": true, ".avif": true, ".txt": true,
	}
	excluded := map[string]bool{
		"node_modules": true, "vendor": true, "venv": true,
		"__pycache__": true, "coverage": true,
	}
	type datedEntry struct {
		file cmuxFileEntry
		time time.Time
	}
	files := make([]datedEntry, 0, maxResults)
	deadline := time.Now().Add(1500 * time.Millisecond)
	inspected := 0
	truncated := false
	depthLimit := maxDepth
	if root == "/" {
		depthLimit = 0
	} else if home, err := os.UserHomeDir(); err == nil {
		if realHome, err := filepath.EvalSymlinks(home); err == nil && realHome == root {
			depthLimit = 0
		}
	}
	var visit func(string, int)
	visit = func(current string, depth int) {
		if inspected >= maxEntries || time.Now().After(deadline) {
			truncated = true
			return
		}
		dir, err := openCmuxDirectory(root, current)
		if err != nil {
			return // 本机目录在扫描时改变或变成符号链接，直接跳过。
		}
		defer dir.Close()
		for {
			batch, err := dir.ReadDir(128)
			for _, item := range batch {
				inspected++
				if inspected > maxEntries || time.Now().After(deadline) {
					truncated = true
					return
				}
				name := item.Name()
				if strings.HasPrefix(name, ".") {
					continue
				}
				var stat unix.Stat_t
				if unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil {
					continue
				}
				next := filepath.Join(current, name)
				switch stat.Mode & unix.S_IFMT {
				case unix.S_IFDIR:
					if depth < depthLimit && !excluded[name] {
						visit(next, depth+1)
					}
				case unix.S_IFREG:
					extension := strings.ToLower(filepath.Ext(name))
					if !allowed[extension] || stat.Size > 20<<20 ||
						(stat.Size > 1<<20 && (extension == ".html" || extension == ".htm" ||
							extension == ".md" || extension == ".markdown" || extension == ".txt")) {
						continue
					}
					modified := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC()
					files = append(files, datedEntry{
						file: cmuxFileEntry{
							Name: name, Path: filepath.Join(root, next), Type: "file",
							Size: stat.Size, ModifiedAt: modified.Format(time.RFC3339Nano),
						},
						time: modified,
					})
				}
			}
			if err == io.EOF || len(batch) == 0 {
				break
			}
			if err != nil {
				return
			}
		}
	}
	visit(relative, 0)
	sort.Slice(files, func(i, j int) bool {
		if files[i].time.Equal(files[j].time) {
			return files[i].file.Path < files[j].file.Path
		}
		return files[i].time.After(files[j].time)
	})
	if len(files) > maxResults {
		files = files[:maxResults]
	}
	result := make([]cmuxFileEntry, 0, len(files))
	for _, entry := range files {
		result = append(result, entry.file)
	}
	return protocol.MustMarshal(struct {
		Files     []cmuxFileEntry `json:"files"`
		Root      string          `json:"root"`
		Truncated bool            `json:"truncated"`
	}{Files: result, Root: filepath.Join(root, relative), Truncated: truncated}), nil
}
