package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
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

func TestBackgroundTextCollector_FallsBackForCodexResultWithoutText(t *testing.T) {
	collector := backgroundTextCollector{}
	collector.add(ClaudeEvent{Type: EventText, Text: "第一项完成。"})
	collector.add(ClaudeEvent{Type: EventText, Text: "第二项完成。"})
	collector.add(ClaudeEvent{Type: EventResult})

	if got, want := collector.text(), "第一项完成。\n\n第二项完成。"; got != want {
		t.Fatalf("text() = %q, want %q", got, want)
	}
}

func TestBackgroundAckRemovesPersistedResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config := DefaultConfig()
	config.Client.ID = "ack-test"
	client := NewClient(config)
	client.savePendingResult(protocol.BackgroundResultPayload{TaskID: "task-ack", CreatedAt: time.Now().UnixMilli()})
	path := filepath.Join(client.pendingResultsDir(), "task-ack.json")
	message, _ := protocol.NewMessage(protocol.TypeBackgroundAck, protocol.BackgroundAckPayload{TaskID: "task-ack"})
	client.handleMessage(message)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pending result should be removed after ACK, err=%v", err)
	}
}

func TestResendPendingResultsRemovesExpiredResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config := DefaultConfig()
	config.Client.ID = "expired-test"
	client := NewClient(config)
	client.savePendingResult(protocol.BackgroundResultPayload{TaskID: "expired", CreatedAt: time.Now().Add(-backgroundResultRetryTTL - time.Second).UnixMilli()})
	client.resendPendingResults()
	if _, err := os.Stat(filepath.Join(client.pendingResultsDir(), "expired.json")); !os.IsNotExist(err) {
		t.Fatalf("expired pending result should be removed, err=%v", err)
	}
}
