package client

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shayin/claude-forward/internal/protocol"
)

func TestCmuxEventsResumeAndGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmux.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan cmuxEventSubscribe, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				_ = conn.Close()
				return
			}
			var subscription cmuxEventSubscribe
			_ = json.Unmarshal(line, &subscription)
			requests <- subscription
			if i == 0 {
				_, _ = conn.Write([]byte("{\"type\":\"ack\",\"resume\":{\"gap\":true}}\n{\"type\":\"event\",\"seq\":8,\"category\":\"terminal\",\"surface_id\":\"s\"}\n"))
			} else {
				_, _ = conn.Write([]byte("{\"type\":\"ack\",\"resume\":{\"gap\":false}}\n{\"type\":\"event\",\"seq\":8}\n{\"type\":\"event\",\"seq\":9,\"category\":\"workspace\",\"workspace_id\":\"w\"}\n"))
			}
			_ = conn.Close()
		}
	}()

	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: path})
	t.Setenv("CMUX_SOCKET_PASSWORD", "local-secret")
	var events []protocol.CmuxEventPayload
	var sequence uint64
	for i := 0; i < 2; i++ {
		acked, _ := manager.readEvents(context.Background(), sequence,
			func(event protocol.CmuxEventPayload) { events = append(events, event) }, &sequence)
		if !acked {
			t.Fatal("event stream did not acknowledge")
		}
		subscription := <-requests
		if subscription.Method != "events.stream" || subscription.Params.AfterSeq != uint64(i*8) ||
			subscription.Params.Password != "local-secret" || len(subscription.Params.Categories) == 0 {
			t.Fatalf("wrong subscribe request: %+v", subscription)
		}
	}
	if sequence != 9 || len(events) != 4 || !events[0].Resync || !events[0].Gap ||
		events[1].SurfaceID != "s" || !events[2].Resync || events[2].Gap ||
		events[3].WorkspaceID != "w" {
		t.Fatalf("unexpected event/replay handling: seq=%d events=%+v", sequence, events)
	}
}

func TestCmuxProgramEventOnlyForwardsBoundedStatusMetadata(t *testing.T) {
	dir, err := os.MkdirTemp("", "cf-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cmux.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(
			`{"type":"ack"}` + "\n" +
				`{"type":"event","seq":1,"category":"program","name":"program_status.changed",` +
				`"workspace_id":"w","surface_id":"s","payload":{"records":[` +
				`{"id":"job","state":"working","title":"构建\n完成",` +
				`"msg":"进度","progress":20,"secret":"do-not-forward"},` +
				`{"state":"unknown","title":"ignore"}]}}` + "\n"))
	}()
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: path})
	var events []protocol.CmuxEventPayload
	var sequence uint64
	_, _ = manager.readEvents(context.Background(), 0,
		func(event protocol.CmuxEventPayload) { events = append(events, event) }, &sequence)
	if len(events) != 2 || len(events[1].Programs) != 1 ||
		events[1].Programs[0].Title != "构建完成" ||
		*events[1].Programs[0].Progress != 20 {
		t.Fatalf("program metadata missing: %+v", events)
	}
	wire, _ := json.Marshal(events[1])
	if strings.Contains(string(wire), "do-not-forward") {
		t.Fatalf("unrecognized payload leaked to Web: %s", wire)
	}
}

func TestCmuxEventQueueAnnouncesGapAfterOverflow(t *testing.T) {
	send := make(chan *protocol.Message, 2)
	occupied, _ := protocol.NewMessage(protocol.TypePing, struct{}{})
	send <- occupied
	send <- occupied
	dropped := false
	queueCmuxEvent(send, &dropped, protocol.CmuxEventPayload{Sequence: 1})
	if !dropped {
		t.Fatal("full queue was not tracked")
	}
	<-send
	<-send
	queueCmuxEvent(send, &dropped, protocol.CmuxEventPayload{Sequence: 2})
	var gap, latest protocol.CmuxEventPayload
	_ = (<-send).ParsePayload(&gap)
	_ = (<-send).ParsePayload(&latest)
	if dropped || !gap.Gap || !gap.Resync || latest.Sequence != 2 {
		t.Fatalf("gap/latest not delivered after overflow: %+v %+v", gap, latest)
	}
}
