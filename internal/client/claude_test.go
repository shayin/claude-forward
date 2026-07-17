package client

import "testing"

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
