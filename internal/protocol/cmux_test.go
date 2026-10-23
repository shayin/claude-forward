package protocol

import (
	"strings"
	"testing"
)

func TestValidateCmuxRequest(t *testing.T) {
	valid := []CmuxRequestPayload{
		{RequestID: "r", Action: CmuxList},
		{RequestID: "r", Action: CmuxWorkspaceReorder, WorkspaceID: "w", Index: 0},
		{RequestID: "r", Action: CmuxGroupCreate, Title: "开发", Directory: "/tmp"},
		{RequestID: "r", Action: CmuxWorkspaceCreate, Title: "新工作区", Directory: "/tmp"},
		{RequestID: "r", Action: CmuxWorkspaceCreate, Title: "Claude", Directory: "/tmp", Profile: "claude"},
		{RequestID: "r", Action: CmuxWorkspaceCreate, Title: "Codex", Directory: "/tmp", Profile: "codex"},
		{RequestID: "r", Action: CmuxTerminalClose, WorkspaceID: "w", SurfaceID: "s"},
		{RequestID: "r", Action: CmuxMouse, WorkspaceID: "w", SurfaceID: "s", Column: 0, Row: 0},
		{RequestID: "r", Action: CmuxFileRoots},
		{RequestID: "r", Action: CmuxFileRecent, Path: "/tmp"},
		{RequestID: "r", Action: CmuxFileList, Path: "/tmp", Query: "test"},
		{RequestID: "r", Action: CmuxFileRead, Path: "/tmp/a", Offset: 5, ReadBytes: 256 << 10},
		{RequestID: "r", Action: CmuxUploadBegin, UploadKind: "attachment", WorkspaceID: "w",
			SurfaceID: "s", FileName: "image.png", Size: 20 << 20},
		{RequestID: "r", Action: CmuxUploadChunk, UploadKind: "file", UploadID: "upload",
			Data: []byte("chunk")},
		{RequestID: "r", Action: CmuxUploadFinish, UploadKind: "file", UploadID: "upload"},
		{RequestID: "r", Action: CmuxBrowserPorts, WorkspaceID: "w"},
		{RequestID: "r", Action: CmuxBrowserNavigate, WorkspaceID: "w",
			Port: 3000, BrowserPath: "/index.html?x=1"},
		{RequestID: "r", Action: CmuxBrowserClick, WorkspaceID: "w", X: 0, Y: 1},
		{RequestID: "r", Action: CmuxBrowserType, WorkspaceID: "w", Text: "中文"},
		{RequestID: "r", Action: CmuxBrowserKey, WorkspaceID: "w", Key: "Enter"},
		{RequestID: "r", Action: CmuxBrowserScroll, WorkspaceID: "w", ScrollY: -500},
		{RequestID: "r", Action: CmuxBrowserNav, WorkspaceID: "w", Key: "reload"},
	}
	for _, request := range valid {
		if err := ValidateCmuxRequest(request); err != nil {
			t.Fatalf("%s should be valid: %v", request.Action, err)
		}
	}

	invalid := []CmuxRequestPayload{
		{Action: CmuxList},
		{RequestID: "r", Action: CmuxFileList, Path: "../etc"},
		{RequestID: "r", Action: CmuxFileRead, Path: "/tmp/a", ReadBytes: 256<<10 + 1},
		{RequestID: "r", Action: CmuxFileRead, Path: "/tmp/a", Offset: -1, ReadBytes: 10},
		{RequestID: "r", Action: CmuxUploadBegin, UploadKind: "attachment", FileName: "a", Size: 1},
		{RequestID: "r", Action: CmuxUploadBegin, UploadKind: "file", Path: "/tmp",
			FileName: "../secret", Size: 1},
		{RequestID: "r", Action: CmuxUploadBegin, UploadKind: "file", Path: "/tmp",
			FileName: "a", Size: 20<<20 + 1},
		{RequestID: "r", Action: CmuxUploadChunk, UploadKind: "file", UploadID: "upload",
			Data: make([]byte, 256<<10+1)},
		{RequestID: "r", Action: "system.exec"},
		{RequestID: "r", Action: CmuxKey, SurfaceID: "s", Key: "return\nunsafe"},
		{RequestID: "r", Action: CmuxText, SurfaceID: "s", Text: strings.Repeat("x", 64<<10+1)},
		{RequestID: "r", Action: CmuxWorkspaceRename, WorkspaceID: "w", Title: "bad\nname"},
		{RequestID: "r", Action: CmuxGroupCreate, Title: "开发", Directory: "bad\x00path"},
		{RequestID: "r", Action: CmuxWorkspaceCreate, Title: "新工作区"},
		{RequestID: "r", Action: CmuxWorkspaceCreate, Title: "危险启动", Directory: "/tmp",
			Profile: "claude --dangerously-skip-permissions"},
		{RequestID: "r", Action: CmuxWorkspaceReorder, WorkspaceID: "w", Index: -1},
		{RequestID: "r", Action: CmuxScroll, WorkspaceID: "w", SurfaceID: "s", DeltaLines: 1001},
		{RequestID: "r", Action: CmuxMouse, WorkspaceID: "w", SurfaceID: "s", Column: -1},
		{RequestID: "r", Action: CmuxNotificationRead},
		{RequestID: "r", Action: CmuxBrowserNavigate, WorkspaceID: "w", Port: 0},
		{RequestID: "r", Action: CmuxBrowserNavigate, WorkspaceID: "w",
			Port: 3000, BrowserPath: "https://example.com"},
		{RequestID: "r", Action: CmuxBrowserNavigate, WorkspaceID: "w",
			Port: 3000, BrowserPath: "/ok#fragment"},
		{RequestID: "r", Action: CmuxBrowserClick, WorkspaceID: "w", X: -0.1},
		{RequestID: "r", Action: CmuxBrowserClick, WorkspaceID: "w", Y: 1.1},
		{RequestID: "r", Action: CmuxBrowserType, WorkspaceID: "w", Text: strings.Repeat("x", 4097)},
		{RequestID: "r", Action: CmuxBrowserKey, WorkspaceID: "w", Key: "Ctrl+L"},
		{RequestID: "r", Action: CmuxBrowserScroll, WorkspaceID: "w", ScrollY: 2001},
		{RequestID: "r", Action: CmuxBrowserNav, WorkspaceID: "w", Key: "open"},
	}
	for _, request := range invalid {
		if err := ValidateCmuxRequest(request); err == nil {
			t.Fatalf("%s should be invalid", request.Action)
		}
	}
}
