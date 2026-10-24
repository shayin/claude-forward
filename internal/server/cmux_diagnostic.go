package server

import (
	"encoding/json"
	"log"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

// 只接受固定字段与有限数值；Server 日志不记录用户发送的自由文本。
type cmuxScrollDiagnostic struct {
	Mode            string `json:"mode"`
	DurationMS      int    `json:"duration_ms"`
	Moves           int    `json:"moves"`
	DeltaPX         int    `json:"delta_px"`
	QueuedPeak      int    `json:"queued_lines_peak"`
	Batches         int    `json:"batches"`
	RPCTotalMS      int    `json:"rpc_ms_total"`
	RPCMaxMS        int    `json:"rpc_ms_max"`
	Failures        int    `json:"failures"`
	SnapshotCount   int    `json:"snapshot_count"`
	SnapshotMaxMS   int    `json:"snapshot_ms_max"`
	RenderMaxMS     int    `json:"render_ms_max"`
	WriteMaxMS      int    `json:"write_ms_max"`
	FirstWriteMS    int    `json:"first_write_ms"`
	DroppedFrames   int    `json:"dropped_frames"`
	UnchangedFrames int    `json:"unchanged_frames"`
}

func parseCmuxScrollDiagnostic(raw json.RawMessage) (cmuxScrollDiagnostic, bool) {
	var d cmuxScrollDiagnostic
	if len(raw) > 1024 || json.Unmarshal(raw, &d) != nil {
		return d, false
	}
	if d.Mode != "remote" && d.Mode != "local" {
		return d, false
	}
	for _, n := range []int{d.DurationMS, d.Moves, d.QueuedPeak, d.Batches,
		d.RPCTotalMS, d.RPCMaxMS, d.Failures, d.SnapshotCount, d.SnapshotMaxMS,
		d.RenderMaxMS, d.WriteMaxMS, d.FirstWriteMS, d.DroppedFrames, d.UnchangedFrames} {
		if n < 0 || n > 60000 {
			return d, false
		}
	}
	if d.DeltaPX < -100000 || d.DeltaPX > 100000 || d.QueuedPeak > 32 ||
		d.DurationMS < 1 || d.Moves > 10000 || d.Batches > 10000 ||
		d.Failures > d.Batches || d.RPCMaxMS > d.RPCTotalMS {
		return d, false
	}
	return d, true
}

func (h *Handler) handleCmuxDiagnostic(conn *Connection, msg *protocol.Message) {
	if conn.Type != ConnTypeUser || !h.hub.HasCmuxAttachment(conn.ID) {
		return
	}
	d, ok := parseCmuxScrollDiagnostic(msg.Payload)
	if !ok {
		return
	}
	conn.mu.Lock()
	if time.Since(conn.lastCmuxDiagnostic) < 2*time.Second {
		conn.mu.Unlock()
		return
	}
	conn.lastCmuxDiagnostic = time.Now()
	conn.mu.Unlock()
	log.Printf("[cmux-scroll] mode=%s duration_ms=%d moves=%d delta_px=%d queue_peak=%d batches=%d rpc_total_ms=%d rpc_max_ms=%d failures=%d snapshots=%d snapshot_max_ms=%d render_max_ms=%d write_max_ms=%d first_write_ms=%d dropped=%d unchanged=%d",
		d.Mode, d.DurationMS, d.Moves, d.DeltaPX, d.QueuedPeak, d.Batches,
		d.RPCTotalMS, d.RPCMaxMS, d.Failures, d.SnapshotCount,
		d.SnapshotMaxMS, d.RenderMaxMS, d.WriteMaxMS, d.FirstWriteMS, d.DroppedFrames, d.UnchangedFrames)
}
