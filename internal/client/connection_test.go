package client

import "testing"

func TestBackgroundTextCollector_PrefersFinalResult(t *testing.T) {
	collector := backgroundTextCollector{}
	collector.add(ClaudeEvent{Type: EventText, Text: "我先检查资料。"})
	collector.add(ClaudeEvent{Type: EventText, Text: "我继续读取第 2 块。"})
	collector.add(ClaudeEvent{Type: EventResult, Text: "全部完成，最终结论如下。"})

	if got, want := collector.text(), "全部完成，最终结论如下。"; got != want {
		t.Fatalf("text() = %q, want %q", got, want)
	}
}

func TestBackgroundTextCollector_FallsBackForCodexResultWithoutText(t *testing.T) {
	collector := backgroundTextCollector{}
	collector.add(ClaudeEvent{Type: EventText, Text: "第一项完成。"})
	collector.add(ClaudeEvent{Type: EventText, Text: "第二项完成。"})
	collector.add(ClaudeEvent{Type: EventResult})

	if got, want := collector.text(), "第一项完成。\n\n第二项完成。"; got != want {
		t.Fatalf("text() = %q, want %q", got, want)
	}
}
