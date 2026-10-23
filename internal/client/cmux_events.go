package client

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/shayin/claude-forward/internal/protocol"
)

type cmuxEventFrame struct {
	Type        string `json:"type"`
	Sequence    uint64 `json:"seq"`
	Category    string `json:"category"`
	Name        string `json:"name"`
	WorkspaceID string `json:"workspace_id"`
	SurfaceID   string `json:"surface_id"`
	Payload     struct {
		Records []protocol.CmuxProgramRecord `json:"records"`
	} `json:"payload"`
	Resume struct {
		Gap bool `json:"gap"`
	} `json:"resume"`
}

type cmuxEventSubscribe struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params struct {
		AfterSeq   uint64   `json:"after_seq"`
		Categories []string `json:"categories"`
		Password   string   `json:"password,omitempty"`
	} `json:"params"`
}

// watchCmuxEvents 连接真实 socket 的 events.stream，断线从最后处理的 seq 续传。
// 事件只用于通知网页刷新，终端正文仍从 terminal.replay 获取。
func (c *Client) watchCmuxEvents(ctx context.Context) {
	var sequence uint64
	backoff := 500 * time.Millisecond
	droppedEvent := false
	for ctx.Err() == nil {
		acknowledged, err := c.cmux.readEvents(ctx, sequence, func(event protocol.CmuxEventPayload) {
			queueCmuxEvent(c.send, &droppedEvent, event)
		}, &sequence)
		if ctx.Err() != nil {
			return
		}
		if acknowledged {
			backoff = 500 * time.Millisecond
		}
		_ = err // socket 不可用时安静退避；cmux 入口发起 list 会得到明确错误。
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func queueCmuxEvent(send chan *protocol.Message, dropped *bool, event protocol.CmuxEventPayload) {
	if *dropped {
		gap, _ := protocol.NewMessage(protocol.TypeCmuxEvent,
			protocol.CmuxEventPayload{Resync: true, Gap: true})
		select {
		case send <- gap:
			*dropped = false
		default:
			return
		}
	}
	message, _ := protocol.NewMessage(protocol.TypeCmuxEvent, event)
	select {
	case send <- message:
	default: // 高速事件不能阻塞聊天/权限链路；下一条带 gap，网页还会 TTL 清理。
		*dropped = true
	}
}

func (m *CmuxManager) readEvents(ctx context.Context, after uint64,
	emit func(protocol.CmuxEventPayload), sequence *uint64) (bool, error) {
	path, err := m.socketPath()
	if err != nil {
		return false, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	subscribe := cmuxEventSubscribe{ID: uuid.NewString(), Method: "events.stream"}
	subscribe.Params.AfterSeq = after
	subscribe.Params.Categories = []string{"workspace", "surface", "pane", "sidebar",
		"notification", "terminal", "agent", "feed", "program"}
	subscribe.Params.Password = os.Getenv("CMUX_SOCKET_PASSWORD")
	wire, err := json.Marshal(subscribe)
	if err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn.Write(append(wire, '\n')); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	acknowledged := false
	lastEmit := make(map[string]time.Time)
	for scanner.Scan() {
		var frame cmuxEventFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		switch frame.Type {
		case "ack":
			acknowledged = true
			emit(protocol.CmuxEventPayload{Resync: true, Gap: frame.Resume.Gap})
		case "event":
			if !acknowledged || frame.Sequence <= *sequence {
				continue
			}
			*sequence = frame.Sequence
			if len(frame.Category) > 64 || len(frame.Name) > 128 ||
				len(frame.WorkspaceID) > 128 || len(frame.SurfaceID) > 128 {
				continue
			}
			now := time.Now()
			if frame.Name != "program_status.changed" &&
				now.Sub(lastEmit[frame.Category]) < 100*time.Millisecond {
				continue
			}
			lastEmit[frame.Category] = now
			event := protocol.CmuxEventPayload{
				Sequence: frame.Sequence, Category: frame.Category, Name: frame.Name,
				WorkspaceID: frame.WorkspaceID, SurfaceID: frame.SurfaceID,
			}
			if frame.Name == "program_status.changed" &&
				frame.WorkspaceID != "" && frame.SurfaceID != "" {
				event.Programs = protocol.CleanCmuxPrograms(frame.Payload.Records)
				if event.Programs == nil {
					event.Programs = []protocol.CmuxProgramRecord{}
				}
			}
			emit(event)
		case "error":
			return acknowledged, io.ErrUnexpectedEOF
		}
	}
	if err := scanner.Err(); err != nil {
		return acknowledged, err
	}
	return acknowledged, io.EOF
}
