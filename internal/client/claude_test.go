package client

import (
	"os"
	"testing"
)

func TestParseJSONLLine_ResultErrorIsNotReportedAsSuccess(t *testing.T) {
	events := parseJSONLLine(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","session_id":"session-1"}`)
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].Type != EventError {
		t.Fatalf("event type = %q, want %q", events[0].Type, EventError)
	}
	if events[0].Text != "Claude 执行未完成（error_max_turns）" {
		t.Fatalf("error text = %q", events[0].Text)
	}
}

func TestParseJSONLLine_ResultSuccessStaysResult(t *testing.T) {
	events := parseJSONLLine(`{"type":"result","subtype":"success","result":"最终答案","session_id":"session-1"}`)
	if len(events) != 1 || events[0].Type != EventResult || events[0].Text != "最终答案" {
		t.Fatalf("events = %+v, want successful result", events)
	}
}

func TestParseJSONLLine_ResultErrorSubtypeWithoutIsError(t *testing.T) {
	events := parseJSONLLine(`{"type":"result","subtype":"error_during_execution","result":"执行中断"}`)
	if len(events) != 1 || events[0].Type != EventError || events[0].Text != "执行中断" {
		t.Fatalf("events = %+v, want result error", events)
	}
}

// TestParseEnvFile_SupportsQuotedAndUnquotedValues 验证 env_file 解析兼容
// 双引号、单引号、无引号三种写法（回归：旧正则强制要引号，导致无引号的
// ANTHROPIC_API_KEY 漏注入，claude 子进程报 Not logged in）。
func TestParseEnvFile_SupportsQuotedAndUnquotedValues(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env.sh"
	content := "#!/bin/bash\n" +
		"# 这是注释\n" +
		"export ANTHROPIC_BASE_URL=\"https://api.example.com/anthropic\"\n" +
		"export ANTHROPIC_API_KEY=sk-unquoted-key-123\n" +
		"export DISABLE_TELEMETRY=1\n" +
		"export SINGLE_QUOTED='hello world'\n" +
		"\n" +
		"export PATH_LIKE=/usr/local/bin:/usr/bin\n" +
		"  export INDENTED=ok\n" +
		"plain line without export\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	got := parseEnvFile(path)
	want := map[string]string{
		"ANTHROPIC_BASE_URL": "https://api.example.com/anthropic",
		"ANTHROPIC_API_KEY":  "sk-unquoted-key-123",
		"DISABLE_TELEMETRY":  "1",
		"SINGLE_QUOTED":      "hello world",
		"PATH_LIKE":          "/usr/local/bin:/usr/bin",
		"INDENTED":           "ok",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d vars, want %d; got=%+v", len(got), len(want), got)
	}
	for k, wantV := range want {
		if gotV := got[k]; gotV != wantV {
			t.Errorf("key %q = %q, want %q", k, gotV, wantV)
		}
	}
}

func TestParseEnvFile_QuotedValueKeepsHashAndSpecialChars(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/env.sh"
	content := "export WITH_HASH=\"a=b#c\"\n" +
		"export URL=\"https://host/path?q=1&x=2\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	got := parseEnvFile(path)
	if got["WITH_HASH"] != "a=b#c" {
		t.Errorf("WITH_HASH = %q, want %q", got["WITH_HASH"], "a=b#c")
	}
	if got["URL"] != "https://host/path?q=1&x=2" {
		t.Errorf("URL = %q", got["URL"])
	}
}

func TestParseEnvFile_MissingFileReturnsNil(t *testing.T) {
	if got := parseEnvFile("/nonexistent/path/env.sh"); got != nil {
		t.Fatalf("expected nil for missing file, got %+v", got)
	}
}
