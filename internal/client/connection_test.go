package client

import (
	"github.com/shayin/claude-forward/internal/protocol"
	"os"
	"path/filepath"
	"testing"
)

func TestBackgroundTextCollector_PrefersFinalResult(t *testing.T) {
	collector := backgroundTextCollector{}
	collector.add(ClaudeEvent{Type: EventText, Text: "我先检查资料。"})
	collector.add(ClaudeEvent{Type: EventText, Text: "我继续读取第 2 块。"})
	collector.add(ClaudeEvent{Type: EventResult, Text: "全部完成，最终结论如下。"})

	if got, want := collector.text(), "全部完成，最终结论如下。"; got != want {
		t.Fatalf("text() = %q, want %q", got, want)
	}
}

func TestBackgroundAckRemovesPersistedResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config := DefaultConfig()
	config.Client.ID = "ack-test"
	client := NewClient(config)
	result := protocol.BackgroundResultPayload{TaskID: "task-ack", CreatedAt: 1}
	client.savePendingResult(result)
	path := filepath.Join(client.pendingResultsDir(), "task-ack.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending result not saved: %v", err)
	}
	message, _ := protocol.NewMessage(protocol.TypeBackgroundAck, protocol.BackgroundAckPayload{TaskID: "task-ack"})
	client.handleMessage(message)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pending result should be removed after ACK, err=%v", err)
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
