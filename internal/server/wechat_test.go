package server

import "testing"

func TestWechatChatResponseCollectText_PrefersFinalResult(t *testing.T) {
	response := &wechatChatResponse{}
	response.collectText("text", "我先读取材料。")
	response.collectText("text", "我继续处理。")
	response.collectText("result", "最终结果。")

	if got, want := response.FullText, "最终结果。"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}

func TestWechatChatResponseCollectText_EmptyResultKeepsFallback(t *testing.T) {
	response := &wechatChatResponse{}
	response.collectText("text", "Codex 的最终 agent message")
	response.collectText("result", "")

	if got, want := response.FullText, "Codex 的最终 agent message"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}

func TestWechatChatResponseCollectText_WhitespaceResultKeepsFallback(t *testing.T) {
	response := &wechatChatResponse{}
	response.collectText("text", "已有有效文本")
	response.collectText("result", " \n\t ")

	if got, want := response.FullText, "已有有效文本"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}
