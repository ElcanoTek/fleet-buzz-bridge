package main

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const realToken = "test-only-real-token"

// fakeChat records what reached fleet's chat API.
type fakeChat struct {
	got []*http.Request
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.got = append(f.got, r.Clone(r.Context()))
	if r.URL.Path == "/chat" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Fleet-Conversation-Id", "conv-1")
		_, _ = io.WriteString(w, "event: text.delta\ndata: {\"text\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "event: turn.completed\ndata: {}\n\n")
		return
	}
	_, _ = io.WriteString(w, "ok")
}

func start(t *testing.T) (*fakeChat, *httptest.Server) {
	t.Helper()
	chat := &fakeChat{}
	upstream := httptest.NewServer(chat)
	t.Cleanup(upstream.Close)
	up, _ := url.Parse(upstream.URL)
	gw := httptest.NewServer(newGateway(up, realToken, "buzz-bot@example.com", log.New(io.Discard, "", 0)))
	t.Cleanup(gw.Close)
	return chat, gw
}

// Whatever the pod sends, fleet sees the real token and the one pinned user;
// no other trust header gets through.
func TestTheTokenAndUserArePinned(t *testing.T) {
	chat, gw := start(t)
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/me", nil)
	req.Header.Set("X-Chat-Server-Token", "whatever-the-pod-has")
	req.Header.Set("X-User-Email", "admin@example.com")
	req.Header.Set("X-User-Session-Epoch", "1")
	req.Header.Set("X-Fleet-Client", "fleet-acp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(chat.got) != 1 {
		t.Fatalf("%d requests reached fleet", len(chat.got))
	}
	h := chat.got[0].Header
	if h.Get("X-Chat-Server-Token") != realToken || h.Get("X-User-Email") != "buzz-bot@example.com" {
		t.Errorf("fleet got token %q as %q", h.Get("X-Chat-Server-Token"), h.Get("X-User-Email"))
	}
	if h.Get("X-User-Session-Epoch") != "" {
		t.Error("a trust header from the pod reached fleet")
	}
	if h.Get("X-Fleet-Client") != "fleet-acp" {
		t.Error("an ordinary header was dropped")
	}
}

// Only what fleet acp calls is forwarded.
func TestOnlyFleetACPsCallsAreForwarded(t *testing.T) {
	chat, gw := start(t)
	for _, c := range []struct {
		method, path string
		pass         bool
	}{
		{http.MethodPost, "/chat", true},
		{http.MethodGet, "/me", true},
		{http.MethodGet, "/client-config", true},
		{http.MethodGet, "/healthz", true},
		{http.MethodPost, "/conversations/conv-1/cancel", true},
		{http.MethodGet, "/chat", false},
		{http.MethodPost, "/conversations/conv-1/approvals/a1", false},
		{http.MethodPost, "/conversations/a/b/cancel", false},
		{http.MethodGet, "/admin/users", false},
		{http.MethodPost, "/conversations/conv-1%2Fx/cancel", false},
	} {
		before := len(chat.got)
		req, _ := http.NewRequest(c.method, gw.URL+c.path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		reached := len(chat.got) > before
		if reached != c.pass || (!c.pass && resp.StatusCode != http.StatusForbidden) {
			t.Errorf("%s %s: reached fleet = %v, status %d; want reached = %v", c.method, c.path, reached, resp.StatusCode, c.pass)
		}
	}
}

// A turn's events arrive as fleet sends them, not when the turn ends.
func TestATurnStreams(t *testing.T) {
	_, gw := start(t)
	start := time.Now()
	resp, err := http.Post(gw.URL+"/chat", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Fleet-Conversation-Id") != "conv-1" {
		t.Error("the conversation header did not come back")
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: text.delta") {
		t.Fatalf("first line %q, %v", line, err)
	}
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Errorf("the first event took %s: the gateway buffered the stream", took)
	}
}

func TestRefusesToStartWithoutItsSettings(t *testing.T) {
	var out strings.Builder
	if code := run([]string{"-listen", "127.0.0.1:0"}, &out); code != 2 {
		t.Errorf("exit %d, want 2 (%s)", code, out.String())
	}
}
