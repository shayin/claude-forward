package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCodexParse_EventTypes 覆盖 codexParser.parse 的主要事件类型映射
func TestCodexParse_EventTypes(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		wantTypes []ClaudeEventType
	}{
		{"thread.started", []string{`{"type":"thread.started","thread_id":"abc-123"}`}, []ClaudeEventType{EventInit}},
		{"agent_message", []string{`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"hi"}}`}, []ClaudeEventType{EventText}},
		{"reasoning", []string{`{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"thinking"}}`}, []ClaudeEventType{EventThinking}},
		{"command_exec pair", []string{
			`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"ls","status":"in_progress"}}`,
			`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls","aggregated_output":"a\nb","exit_code":0,"status":"completed"}}`,
		}, []ClaudeEventType{EventToolStart, EventToolEnd}},
		{"file_change pair", []string{
			`{"type":"item.started","item":{"id":"item_1","type":"file_change","status":"in_progress"}}`,
			`{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
		}, []ClaudeEventType{EventToolStart, EventToolEnd}},
		{"mcp pair", []string{
			`{"type":"item.started","item":{"id":"item_2","type":"mcp_tool_call","server":"s","tool":"foo","status":"in_progress"}}`,
			`{"type":"item.completed","item":{"id":"item_2","type":"mcp_tool_call","server":"s","tool":"foo","result":{"ok":true},"status":"completed"}}`,
		}, []ClaudeEventType{EventToolStart, EventToolEnd}},
		{"turn.completed", []string{`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":50,"reasoning_output_tokens":5}}`}, []ClaudeEventType{EventResult}},
		{"turn.failed", []string{`{"type":"turn.failed","error":{"message":"boom"}}`}, []ClaudeEventType{EventError}},
		{"top-level error", []string{`{"type":"error","message":"fatal"}`}, []ClaudeEventType{EventError}},
		{"item error", []string{`{"type":"item.completed","item":{"id":"item_9","type":"error","message":"warn"}}`}, []ClaudeEventType{EventError}},
		{"turn.completed no usage", []string{`{"type":"turn.completed"}`}, []ClaudeEventType{EventResult}},
		{"web_search pair", []string{
			`{"type":"item.started","item":{"id":"i3","type":"web_search","status":"in_progress"}}`,
			`{"type":"item.completed","item":{"id":"i3","type":"web_search","query":"q","status":"completed"}}`,
		}, []ClaudeEventType{EventToolStart, EventToolEnd}},
		{"turn.started ignored", []string{`{"type":"turn.started"}`}, nil},
		{"invalid json ignored", []string{`not json`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &codexParser{}
			var got []ClaudeEvent
			for _, line := range tt.lines {
				got = append(got, p.parse(line)...)
			}
			if len(got) != len(tt.wantTypes) {
				types := make([]string, len(got))
				for i, e := range got {
					types[i] = string(e.Type)
				}
				t.Fatalf("got %d events %v, want %d", len(got), types, len(tt.wantTypes))
			}
			for i, want := range tt.wantTypes {
				if got[i].Type != want {
					t.Errorf("event[%d].Type = %q, want %q", i, got[i].Type, want)
				}
			}
		})
	}
}

// TestCodexParse_Fields 验证关键字段填充与边界行为
func TestCodexParse_Fields(t *testing.T) {
	// thread.started 填充 SessionID
	p := &codexParser{}
	if evs := p.parse(`{"type":"thread.started","thread_id":"tid-1"}`); len(evs) != 1 || evs[0].SessionID != "tid-1" {
		t.Fatalf("thread.started SessionID: %v", p.parse(`{"type":"thread.started","thread_id":"tid-1"}`))
	}

	// resume thread_id 不匹配：不崩溃，EventInit 用返回的 thread_id
	p2 := &codexParser{expectedThread: "expected"}
	if evs := p2.parse(`{"type":"thread.started","thread_id":"actual"}`); len(evs) != 1 || evs[0].Type != EventInit || evs[0].SessionID != "actual" {
		t.Fatalf("resume mismatch should not crash, use returned thread_id: %v", evs)
	}

	// turn.completed token 字段（OutputTokens 含 reasoning_output_tokens）
	p3 := &codexParser{}
	evs := p3.parse(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":5,"output_tokens":3,"reasoning_output_tokens":2}}`)
	if len(evs) != 1 || evs[0].InputTokens != 10 || evs[0].OutputTokens != 5 || evs[0].CacheReadInputTokens != 5 || evs[0].CostUSD != 0 {
		t.Fatalf("tokens mapping: %+v", evs)
	}

	// command_execution completed 的 ToolOutput = aggregated_output
	p4 := &codexParser{}
	p4.parse(`{"type":"item.started","item":{"id":"i0","type":"command_execution","command":"echo hi","status":"in_progress"}}`)
	if evs := p4.parse(`{"type":"item.completed","item":{"id":"i0","type":"command_execution","command":"echo hi","aggregated_output":"hi","exit_code":0,"status":"completed"}}`); len(evs) != 1 || evs[0].ToolOutput != "hi" || evs[0].ToolName != "shell" {
		t.Fatalf("ToolOutput mapping: %+v", evs)
	}

	// 非 completed 状态的 command_execution，ToolOutput 带 status 前缀（网络阻断等场景）
	p5 := &codexParser{}
	if evs := p5.parse(`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"curl","aggregated_output":"blocked","exit_code":null,"status":"declined"}}`); len(evs) != 1 || !strings.Contains(evs[0].ToolOutput, "[declined]") {
		t.Fatalf("declined status should prefix ToolOutput: %+v", evs)
	}

	// command_execution started 的 ToolInput 是合法 JSON，含 command
	p6 := &codexParser{}
	evs6 := p6.parse(`{"type":"item.started","item":{"id":"i2","type":"command_execution","command":"ls -la","status":"in_progress"}}`)
	if len(evs6) != 1 {
		t.Fatalf("started event: %v", evs6)
	}
	var cmdField struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(evs6[0].ToolInput, &cmdField); err != nil || cmdField.Command != "ls -la" {
		t.Fatalf("ToolInput should be valid JSON with command: err=%v cmd=%q", err, cmdField.Command)
	}

	// turn.completed usage 为 nil 时仍发空 EventResult（不返回 nil，避免用户卡在无响应）
	p7 := &codexParser{}
	if evs := p7.parse(`{"type":"turn.completed"}`); len(evs) != 1 || evs[0].Type != EventResult || evs[0].InputTokens != 0 {
		t.Fatalf("turn.completed with nil usage should emit empty EventResult: %+v", evs)
	}

	// mcp_tool_call 的 arguments 为空时 ToolInput 用 null（不 Marshal 报错被静默丢弃）
	p8 := &codexParser{}
	evs8 := p8.parse(`{"type":"item.started","item":{"id":"i4","type":"mcp_tool_call","server":"s","tool":"foo","status":"in_progress"}}`)
	if len(evs8) != 1 || !strings.Contains(string(evs8[0].ToolInput), "null") {
		t.Fatalf("empty mcp arguments should yield null in ToolInput: %+v", evs8)
	}

	// web_search 提取 query：ToolStart 带到 ToolInput，ToolEnd 带到 ToolOutput（两端对称）
	p9 := &codexParser{}
	evsStart := p9.parse(`{"type":"item.started","item":{"id":"i5","type":"web_search","query":"golang context","status":"in_progress"}}`)
	if len(evsStart) != 1 || !strings.Contains(string(evsStart[0].ToolInput), "golang context") {
		t.Fatalf("web_search started should carry query in ToolInput: %+v", evsStart)
	}
	evsEnd := p9.parse(`{"type":"item.completed","item":{"id":"i5","type":"web_search","query":"golang context","status":"completed"}}`)
	if len(evsEnd) != 1 || evsEnd[0].ToolOutput != "golang context" {
		t.Fatalf("web_search completed should carry query in ToolOutput: %+v", evsEnd)
	}

	// turn.failed 提取 error.message（单次解析，不依赖第二次 Unmarshal）
	p10 := &codexParser{}
	if evs := p10.parse(`{"type":"turn.failed","error":{"message":"specific failure"}}`); len(evs) != 1 || evs[0].Type != EventError || evs[0].Text != "specific failure" {
		t.Fatalf("turn.failed should extract error.message: %+v", evs)
	}
}

// TestRunnerInterface_Compliance 编译期确认两个引擎都满足 Runner
func TestRunnerInterface_Compliance(t *testing.T) {
	var _ Runner = (*ClaudeManager)(nil)
	var _ Runner = (*CodexManager)(nil)
}
