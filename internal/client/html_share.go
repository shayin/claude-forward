package client

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

const maxSharedFileSize = 16 << 20

type htmlSnapshot map[string]fileFingerprint

type fileFingerprint struct {
	modTime time.Time
	size    int64
}

// HTMLShare 只负责本地目录的访问控制、令牌和链接生成。
type HTMLShare struct {
	rootDir string
	token   string
	baseURL string
}

func newHTMLShare(config *Config) (*HTMLShare, error) {
	if strings.TrimSpace(config.HTMLShare.RootDir) == "" {
		return nil, nil
	}
	rootDir, err := expandAbsPath(config.HTMLShare.RootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve html_share.root_dir: %w", err)
	}
	info, err := os.Stat(rootDir)
	if err != nil {
		return nil, fmt.Errorf("stat html_share.root_dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("html_share.root_dir is not a directory")
	}
	tokenFile := config.HTMLShare.TokenFile
	if tokenFile == "" {
		sum := sha256.Sum256([]byte(config.Client.ID))
		tokenFile = filepath.Join(config.Path, ".claude-forward", "html-share-token-"+base64.RawURLEncoding.EncodeToString(sum[:8]))
	}
	tokenFile, err = expandAbsPath(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("resolve html_share.token_file: %w", err)
	}
	token, err := loadOrCreateShareToken(tokenFile)
	if err != nil {
		return nil, err
	}
	return &HTMLShare{rootDir: rootDir, token: token, baseURL: derivePublicBaseURL(config.Server.URL, config.HTMLShare.PublicBaseURL)}, nil
}

func expandAbsPath(value string) (string, error) {
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	return filepath.Abs(value)
}

func loadOrCreateShareToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(bytes)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

func derivePublicBaseURL(serverURL, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return strings.TrimRight(u.String(), "/")
}

func (s *HTMLShare) link(clientID, relPath string) string {
	if s == nil || s.baseURL == "" {
		return ""
	}
	segments := []string{strings.TrimRight(s.baseURL, "/"), "share", url.PathEscape(clientID), url.PathEscape(s.token)}
	for _, part := range strings.Split(filepath.ToSlash(relPath), "/") {
		segments = append(segments, url.PathEscape(part))
	}
	return strings.Join(segments, "/")
}

func (s *HTMLShare) read(request protocol.FileRequestPayload) protocol.FileResponsePayload {
	response := protocol.FileResponsePayload{RequestID: request.RequestID, Path: request.Path, StatusCode: 404}
	if s == nil || subtle.ConstantTimeCompare([]byte(request.Token), []byte(s.token)) != 1 {
		response.StatusCode = 403
		return response
	}
	path, err := s.resolveFile(request.Path)
	if err != nil {
		return response
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return response
	}
	if info.Size() > maxSharedFileSize {
		response.StatusCode = 413
		return response
	}
	data, err := os.ReadFile(path)
	if err != nil {
		response.StatusCode = 500
		return response
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	response.StatusCode, response.ContentType, response.Content = 200, contentType, data
	return response
}

func (s *HTMLShare) resolveFile(relPath string) (string, error) {
	if relPath == "" || filepath.IsAbs(relPath) {
		return "", fmt.Errorf("invalid path")
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal")
	}
	candidate := filepath.Join(s.rootDir, clean)
	realPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(s.rootDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("symlink escapes root")
	}
	return realPath, nil
}

func (s *HTMLShare) snapshot() htmlSnapshot {
	result := make(htmlSnapshot)
	if s == nil {
		return result
	}
	filepath.WalkDir(s.rootDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !isHTML(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(s.rootDir, path)
		if err == nil {
			result[filepath.ToSlash(rel)] = fileFingerprint{info.ModTime(), info.Size()}
		}
		return nil
	})
	return result
}

func (s *HTMLShare) changedHTML(before htmlSnapshot) []string {
	after := s.snapshot()
	var changed []string
	for path, now := range after {
		if old, ok := before[path]; !ok || old != now {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

func isHTML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".html" || ext == ".htm"
}

func (s *HTMLShare) linksText(clientID string, before htmlSnapshot) string {
	paths := s.changedHTML(before)
	if len(paths) == 0 {
		return ""
	}
	links := make([]string, 0, len(paths))
	for _, path := range paths {
		links = append(links, "HTML 预览链接："+s.link(clientID, path))
	}
	return strings.Join(links, "\n")
}
