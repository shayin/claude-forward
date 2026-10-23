package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type blockedDownloadWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	writes  int
}

func (w *blockedDownloadWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		close(w.entered)
		<-w.release
	}
	return w.ResponseRecorder.Write(data)
}

func (w *blockedDownloadWriter) Flush() { w.ResponseRecorder.Flush() }

func cmuxDownloadRequest(t *testing.T, client *http.Client, method, address, bearer string, body []byte, offset int) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, address, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if offset >= 0 {
		request.Header.Set("X-Download-Offset", strconv.Itoa(offset))
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func cmuxDownloadTicket(t *testing.T, client *http.Client, base, token, name string, size int) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "size": size})
	response := cmuxDownloadRequest(t, client, http.MethodPost, base, token, body, -1)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare: status %d", response.StatusCode)
	}
	var ticket struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&ticket); err != nil || ticket.ID == "" {
		t.Fatal("download ticket missing", err)
	}
	return base + "/" + ticket.ID
}

func TestCmuxDownloadStreamsOnceWithBackpressure(t *testing.T) {
	handler := NewHandler(NewHub(), NewAuth([]string{"one", "two"}), nil)
	server := httptest.NewServer(http.HandlerFunc(handler.HandleCmuxDownload))
	defer server.Close()
	base := server.URL + "/api/cmux-download"
	ticket := cmuxDownloadTicket(t, server.Client(), base, "one", "测试 文件.txt", 5)
	response := cmuxDownloadRequest(t, server.Client(), http.MethodGet, ticket, "", nil, -1)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Length") != "5" ||
		!strings.Contains(response.Header.Get("Content-Disposition"), "%E6%B5%8B%E8%AF%95%20") ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unsafe download headers: %v", response.Header)
	}
	wrong := cmuxDownloadRequest(t, server.Client(), http.MethodPost, ticket+"/chunk", "two", []byte("ab"), 0)
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another valid token may not write: %d", wrong.StatusCode)
	}
	wrong.Body.Close()
	for _, part := range []struct {
		data   string
		offset int
	}{{"ab", 0}, {"cde", 2}} {
		result := cmuxDownloadRequest(t, server.Client(), http.MethodPost,
			ticket+"/chunk", "one", []byte(part.data), part.offset)
		if result.StatusCode != http.StatusNoContent {
			t.Fatalf("chunk %d: %d", part.offset, result.StatusCode)
		}
		result.Body.Close()
	}
	finish := cmuxDownloadRequest(t, server.Client(), http.MethodPost, ticket+"/finish", "one", nil, 5)
	if finish.StatusCode != http.StatusNoContent {
		t.Fatalf("finish: %d", finish.StatusCode)
	}
	finish.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil || string(content) != "abcde" {
		t.Fatalf("download bytes: %q, %v", content, err)
	}
	reused := cmuxDownloadRequest(t, server.Client(), http.MethodGet, ticket, "", nil, -1)
	if reused.StatusCode != http.StatusNotFound {
		t.Fatalf("ticket reused: %d", reused.StatusCode)
	}
	reused.Body.Close()
}

func TestCmuxDownloadRejectsInvalidInputAndOffsets(t *testing.T) {
	handler := NewHandler(NewHub(), NewAuth([]string{"one"}), nil)
	server := httptest.NewServer(http.HandlerFunc(handler.HandleCmuxDownload))
	defer server.Close()
	base := server.URL + "/api/cmux-download"
	unauthorized := cmuxDownloadRequest(t, server.Client(), http.MethodPost, base, "",
		[]byte(`{"name":"ok","size":1}`), -1)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized prepare: %d", unauthorized.StatusCode)
	}
	unauthorized.Body.Close()
	for _, body := range []string{
		`{"name":"../secret","size":2}`, `{"name":"a\r\nb","size":2}`,
		`{"name":"valid","size":1073741825}`, `{"name":"valid","size":-1}`,
	} {
		response := cmuxDownloadRequest(t, server.Client(), http.MethodPost, base, "one", []byte(body), -1)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("accepted bad prepare %s: %d", body, response.StatusCode)
		}
		response.Body.Close()
	}
	ticket := cmuxDownloadTicket(t, server.Client(), base, "one", "ok.txt", 3)
	get := cmuxDownloadRequest(t, server.Client(), http.MethodGet, ticket, "", nil, -1)
	defer get.Body.Close()
	for _, testcase := range []struct {
		suffix string
		body   string
		offset int
	}{
		{"/chunk", "ab", 1}, {"/chunk", "abcd", 0}, {"/finish", "", 0},
	} {
		response := cmuxDownloadRequest(t, server.Client(), http.MethodPost,
			ticket+testcase.suffix, "one", []byte(testcase.body), testcase.offset)
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("accepted invalid offset/size: %d", response.StatusCode)
		}
		response.Body.Close()
	}
	abort := cmuxDownloadRequest(t, server.Client(), http.MethodPost, ticket+"/abort", "one", nil, -1)
	if abort.StatusCode != http.StatusNoContent {
		t.Fatalf("abort: %d", abort.StatusCode)
	}
	abort.Body.Close()
}

func TestCmuxDownloadEmptyFile(t *testing.T) {
	handler := NewHandler(NewHub(), NewAuth([]string{"one"}), nil)
	server := httptest.NewServer(http.HandlerFunc(handler.HandleCmuxDownload))
	defer server.Close()
	ticket := cmuxDownloadTicket(t, server.Client(), server.URL+"/api/cmux-download", "one", "empty.txt", 0)
	response := cmuxDownloadRequest(t, server.Client(), http.MethodGet, ticket, "", nil, -1)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("empty download: status=%d len=%d err=%v", response.StatusCode, len(body), err)
	}
}

func TestCmuxDownloadPendingQuotaAndActiveStream(t *testing.T) {
	handler := NewHandler(NewHub(), NewAuth([]string{"one", "two"}), nil)
	server := httptest.NewServer(http.HandlerFunc(handler.HandleCmuxDownload))
	defer server.Close()
	base := server.URL + "/api/cmux-download"
	tickets := make([]string, 0, cmuxDownloadPerToken)
	for i := 0; i < cmuxDownloadPerToken; i++ {
		tickets = append(tickets, cmuxDownloadTicket(t, server.Client(), base, "one", "test.txt", 1))
	}
	denied := cmuxDownloadRequest(t, server.Client(), http.MethodPost, base,
		"one", []byte(`{"name":"test.txt","size":1}`), -1)
	if denied.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("same token exceeded pending quota: %d", denied.StatusCode)
	}
	denied.Body.Close()
	other := cmuxDownloadTicket(t, server.Client(), base, "two", "other.txt", 0)
	otherResult := cmuxDownloadRequest(t, server.Client(), http.MethodGet, other, "", nil, -1)
	otherResult.Body.Close()
	firstID := strings.TrimPrefix(tickets[0], base+"/")
	handler.cmuxDownloads.expirePending(firstID, handler.cmuxDownloads.get(firstID))
	expired := cmuxDownloadRequest(t, server.Client(), http.MethodGet, tickets[0], "", nil, -1)
	if expired.StatusCode != http.StatusNotFound {
		t.Fatalf("unclaimed ticket still usable: %d", expired.StatusCode)
	}
	expired.Body.Close()
	available := cmuxDownloadTicket(t, server.Client(), base, "one", "freed.txt", 0)
	availableResult := cmuxDownloadRequest(t, server.Client(), http.MethodGet, available, "", nil, -1)
	availableResult.Body.Close()
	active := tickets[1]
	response := cmuxDownloadRequest(t, server.Client(), http.MethodGet, active, "", nil, -1)
	defer response.Body.Close()
	activeID := strings.TrimPrefix(active, base+"/")
	handler.cmuxDownloads.expirePending(activeID, handler.cmuxDownloads.get(activeID))
	chunk := cmuxDownloadRequest(t, server.Client(), http.MethodPost, active+"/chunk", "one", []byte("a"), 0)
	if chunk.StatusCode != http.StatusNoContent {
		t.Fatalf("active download expired despite progress: %d", chunk.StatusCode)
	}
	chunk.Body.Close()
	finish := cmuxDownloadRequest(t, server.Client(), http.MethodPost, active+"/finish", "one", nil, 1)
	if finish.StatusCode != http.StatusNoContent {
		t.Fatalf("active download finish: %d", finish.StatusCode)
	}
	finish.Body.Close()
}

func TestCmuxDownloadAbortWaitsForStartedWrite(t *testing.T) {
	handler := NewHandler(NewHub(), NewAuth([]string{"one"}), nil)
	prepare := httptest.NewRecorder()
	prepareRequest := httptest.NewRequest(http.MethodPost, "/api/cmux-download",
		strings.NewReader(`{"name":"test.txt","size":2}`))
	prepareRequest.Header.Set("Authorization", "Bearer one")
	handler.HandleCmuxDownload(prepare, prepareRequest)
	var ticket struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(prepare.Body.Bytes(), &ticket) != nil || ticket.ID == "" {
		t.Fatal("ticket not created")
	}
	address := "/api/cmux-download/" + ticket.ID
	writer := &blockedDownloadWriter{ResponseRecorder: httptest.NewRecorder(),
		entered: make(chan struct{}), release: make(chan struct{})}
	getDone := make(chan struct{})
	go func() {
		handler.HandleCmuxDownload(writer, httptest.NewRequest(http.MethodGet, address, nil))
		close(getDone)
	}()
	select {
	case <-handler.cmuxDownloads.get(ticket.ID).started:
	case <-time.After(2 * time.Second):
		t.Fatal("GET did not start")
	}
	postDone := make(chan struct{})
	go func() {
		post := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, address+"/chunk", strings.NewReader("ab"))
		request.Header.Set("Authorization", "Bearer one")
		request.Header.Set("X-Download-Offset", "0")
		handler.HandleCmuxDownload(post, request)
		close(postDone)
	}()
	select {
	case <-writer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("GET did not begin writing the chunk")
	}
	abortDone := make(chan int, 1)
	go func() {
		abort := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, address+"/abort", nil)
		request.Header.Set("Authorization", "Bearer one")
		handler.HandleCmuxDownload(abort, request)
		abortDone <- abort.Code
	}()
	select {
	case <-abortDone:
		t.Fatal("abort returned while a network write was still in progress")
	case <-time.After(50 * time.Millisecond):
	}
	close(writer.release)
	select {
	case status := <-abortDone:
		if status != http.StatusNoContent {
			t.Fatalf("abort status: %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not finish after the write released")
	}
	select {
	case <-postDone:
	case <-time.After(2 * time.Second):
		t.Fatal("chunk POST remained blocked")
	}
	select {
	case <-getDone:
	case <-time.After(2 * time.Second):
		t.Fatal("GET remained blocked")
	}
	if writer.writes != 1 {
		t.Fatalf("additional writes after abort: %d", writer.writes)
	}
}
