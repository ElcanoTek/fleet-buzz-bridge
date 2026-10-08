package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the agent the bridge starts: with
// BRIDGE_FAKE_AGENT=1 it speaks just enough ACP, writes what it was asked and
// what it could see into BRIDGE_FAKE_DIR, and answers as BRIDGE_FAKE_MODE says.
func TestMain(m *testing.M) {
	if os.Getenv("BRIDGE_FAKE_AGENT") == "1" {
		fakeAgent()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAgent() {
	dir, mode := os.Getenv("BRIDGE_FAKE_DIR"), os.Getenv("BRIDGE_FAKE_MODE")
	hasKey := "0"
	if os.Getenv("BUZZ_PRIVATE_KEY") != "" || os.Getenv("BUZZ_API_TOKEN") != "" {
		hasKey = "1"
	}
	_ = os.WriteFile(filepath.Join(dir, "agent-has-key"), []byte(hasKey), 0o600)
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewReaderSize(os.Stdin, 1<<16)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return // the bridge closed stdin: the client hung up
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		_ = json.Unmarshal(line, &req)
		reply := func(v map[string]any) { v["jsonrpc"] = "2.0"; v["id"] = req.ID; _ = out.Encode(v) }
		chunk := func(text string) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": req.Params.SessionID,
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}},
			}})
		}
		toolCall := func() {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": req.Params.SessionID,
				"update":    map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "run_python", "status": "in_progress"},
			}})
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{"result": map[string]any{"protocolVersion": 1}})
		case "session/new":
			if mode == "refuse-session" {
				reply(map[string]any{"error": map[string]any{"code": -32000, "message": "server rejected the request (403): nobody@example.com is not a fleet user"}})
				continue
			}
			reply(map[string]any{"result": map[string]any{"sessionId": "s1"}})
		case "session/prompt":
			var text strings.Builder
			for _, b := range req.Params.Prompt {
				text.WriteString(b.Text)
			}
			_ = os.WriteFile(filepath.Join(dir, "agent-prompt"), []byte(text.String()), 0o600)
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": req.Params.SessionID,
				"update":    map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "thinking"}},
			}})
			switch mode {
			case "cancel":
				chunk("partial")
				reply(map[string]any{"result": map[string]any{"stopReason": "cancelled"}})
			case "error":
				reply(map[string]any{"error": map[string]any{"code": -32603, "message": "the fleet turn did not finish within 15s and was stopped"}})
			case "silent":
				reply(map[string]any{"result": map[string]any{"stopReason": "end_turn"}})
			case "narrate":
				chunk("I'll compute 17 × 23 now.")
				toolCall()
				chunk("17 × 23 = 391.")
				reply(map[string]any{"result": map[string]any{"stopReason": "end_turn"}})
			case "late-cancel":
				chunk("albatross")
				chunk("\n\nThe turn had already finished before the Stop reached fleet, so nothing was stopped: what it did (tool calls included) stands. See http://fleet.test/chat?c=1")
				reply(map[string]any{"result": map[string]any{"stopReason": "cancelled"}})
			case "unconfirmed-stop":
				chunk("I'll run that now:")
				toolCall()
				chunk("\n\nfleet could not confirm this turn stopped (timeout). It may still be running: stop it at http://fleet.test/chat?c=1")
				reply(map[string]any{"result": map[string]any{"stopReason": "cancelled"}})
			case "tool-last":
				chunk("Here is what I found.")
				toolCall()
				reply(map[string]any{"result": map[string]any{"stopReason": "end_turn"}})
			default:
				chunk("Hello")
				chunk(" world\n")
				reply(map[string]any{"result": map[string]any{"stopReason": "end_turn"}})
			}
		}
	}
}

type harness struct {
	t      *testing.T
	dir    string
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *strings.Builder
	done   chan int
}

func startBridge(t *testing.T, mode string) *harness {
	t.Helper()
	dir := t.TempDir()
	// A stand-in for the Buzz CLI on the bridge's side: it records how it was
	// called, what it was given to post, and whether it could see the key.
	cli := filepath.Join(dir, "buzz")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + dir + "/buzz-args\"\ncat > \"" + dir + "/buzz-stdin\"\n" +
		"[ -n \"$BUZZ_PRIVATE_KEY\" ] && echo 1 > \"" + dir + "/buzz-has-key\"\nexit 0\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"BRIDGE_FAKE_AGENT=1", "BRIDGE_FAKE_DIR="+dir, "BRIDGE_FAKE_MODE="+mode,
		"BUZZ_PRIVATE_KEY=test-only-not-a-key", "BUZZ_API_TOKEN=test-only-token", "FLEET_BUZZ_CLI="+cli)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{t: t, dir: dir, in: inW, out: bufio.NewReaderSize(outR, 1<<16), stderr: &strings.Builder{}, done: make(chan int, 1)}
	go func() {
		h.done <- run([]string{os.Args[0]}, inR, outW, &syncWriter{b: h.stderr}, env)
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

type syncWriter struct{ b *strings.Builder }

func (w *syncWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func (h *harness) send(id int, method string, params any) {
	h.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

// until reads the bridge's output up to the response with id, returning every
// line on the way.
func (h *harness) until(id int) []map[string]any {
	h.t.Helper()
	var got []map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		line, err := h.out.ReadBytes('\n')
		if err != nil {
			h.t.Fatalf("bridge output ended: %v (stderr: %s)", err, h.stderr)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			h.t.Fatalf("non-JSON on stdout: %q", line)
		}
		got = append(got, m)
		if v, ok := m["id"].(float64); ok && int(v) == id {
			return got
		}
	}
	h.t.Fatal("timed out")
	return nil
}

func (h *harness) promptBuzz(t *testing.T, text string) []map[string]any {
	h.send(1, "initialize", map[string]any{"protocolVersion": 1})
	h.until(1)
	h.send(2, "session/new", map[string]any{"cwd": "/tmp", "mcpServers": []any{}})
	h.until(2)
	h.send(3, "session/prompt", map[string]any{"sessionId": "s1", "prompt": []any{map[string]any{"type": "text", "text": text}}})
	return h.until(3)
}

func (h *harness) file(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(h.dir, name))
	return string(b), err == nil
}

func TestPostsTheAnswerToTheBuzzThread(t *testing.T) {
	h := startBridge(t, "answer")
	lines := h.promptBuzz(t, firstPrompt(t))

	// Posted before buzz-acp is told the turn ended: the files exist as soon
	// as the response arrives.
	args, ok := h.file("buzz-args")
	if !ok {
		t.Fatalf("nothing posted (stderr: %s)", h.stderr)
	}
	want := "messages\nsend\n--channel\n" + fixtureChannel + "\n--content\n-\n--reply-to\n" + fixtureEvent + "\n"
	if args != want {
		t.Errorf("buzz called with\n%s\nwant\n%s", args, want)
	}
	if body, _ := h.file("buzz-stdin"); body != "Hello world" {
		t.Errorf("posted %q, want the streamed answer trimmed", body)
	}
	if k, _ := h.file("buzz-has-key"); strings.TrimSpace(k) != "1" {
		t.Error("the Buzz CLI did not get BUZZ_PRIVATE_KEY: it posts as the agent's identity")
	}
	// The agent never sees Buzz's credentials, nor Buzz's CLI instructions.
	if k, _ := h.file("agent-has-key"); k != "0" {
		t.Error("the agent was given a Buzz credential")
	}
	prompt, _ := h.file("agent-prompt")
	if !strings.Contains(prompt, "alice: @fleet hey") || strings.Contains(prompt, "<base>") || strings.Contains(prompt, "buzz messages send") {
		t.Errorf("agent was asked:\n%s", prompt)
	}
	// Everything still reaches buzz-acp unchanged: the thought, both chunks,
	// and the end of the turn.
	var kinds []string
	for _, m := range lines {
		if p, ok := m["params"].(map[string]any); ok {
			kinds = append(kinds, p["update"].(map[string]any)["sessionUpdate"].(string))
		}
	}
	if strings.Join(kinds, ",") != "agent_thought_chunk,agent_message_chunk,agent_message_chunk" {
		t.Errorf("updates passed on = %v", kinds)
	}
	if r := lines[len(lines)-1]["result"].(map[string]any)["stopReason"]; r != "end_turn" {
		t.Errorf("stopReason passed on = %v", r)
	}
}

func TestACancelledTurnPostsNothing(t *testing.T) {
	h := startBridge(t, "cancel")
	h.promptBuzz(t, firstPrompt(t))
	if _, ok := h.file("buzz-args"); ok {
		t.Error("posted the text of a cancelled turn")
	}
}

func TestAnErrorIsPostedSoTheAskerIsNotLeftWaiting(t *testing.T) {
	h := startBridge(t, "error")
	lines := h.promptBuzz(t, firstPrompt(t))
	if body, _ := h.file("buzz-stdin"); body != "fleet could not answer: the fleet turn did not finish within 15s and was stopped" {
		t.Errorf("posted %q", body)
	}
	// buzz-acp retries a failed prompt, and each retry is a new fleet turn
	// (seen live: a timeout re-ran four times and posted four replies). The
	// asker has been told, so buzz-acp is told the turn ended.
	last := lines[len(lines)-1]
	if _, isErr := last["error"]; isErr {
		t.Error("the error was passed on: buzz-acp would retry the turn")
	}
	if r, _ := last["result"].(map[string]any); r["stopReason"] != "end_turn" {
		t.Errorf("buzz-acp got %v, want the turn reported as ended", last)
	}
}

func TestATurnWithNoAnswerTextPostsNothing(t *testing.T) {
	h := startBridge(t, "silent")
	h.promptBuzz(t, firstPrompt(t))
	if _, ok := h.file("buzz-args"); ok {
		t.Error("posted an empty answer")
	}
}

func TestAPromptNotFromBuzzPassesThrough(t *testing.T) {
	h := startBridge(t, "answer")
	h.promptBuzz(t, "What is 2+2?")
	if prompt, _ := h.file("agent-prompt"); prompt != "What is 2+2?" {
		t.Errorf("agent was asked %q, want the prompt unchanged", prompt)
	}
	if _, ok := h.file("buzz-args"); ok {
		t.Error("posted the answer to a prompt with no Buzz thread")
	}
}

func TestClientHangUpEndsTheAgentAndTheBridge(t *testing.T) {
	h := startBridge(t, "answer")
	h.send(1, "initialize", map[string]any{"protocolVersion": 1})
	h.until(1)
	_ = h.in.Close()
	select {
	case code := <-h.done:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not exit when its client hung up")
	}
}

// fleet keeps only the text after the last tool call as a turn's answer, and
// so does the bridge: narration before a tool call is not posted, nor run
// together with the answer (seen live as "…now.17 × 23 = 391.").
func TestPostsOnlyTheTextAfterTheLastToolCall(t *testing.T) {
	h := startBridge(t, "narrate")
	h.promptBuzz(t, firstPrompt(t))
	if body, _ := h.file("buzz-stdin"); body != "17 × 23 = 391." {
		t.Errorf("posted %q, want only the final answer", body)
	}
}

// With nothing written after the last tool call, the latest text written is
// the answer.
func TestPostsTheLatestTextWhenNothingFollowsTheLastToolCall(t *testing.T) {
	h := startBridge(t, "tool-last")
	h.promptBuzz(t, firstPrompt(t))
	if body, _ := h.file("buzz-stdin"); body != "Here is what I found." {
		t.Errorf("posted %q", body)
	}
}

// A Stop that reached fleet after the turn had finished leaves the answer
// standing: the asker gets it, with fleet's note, rather than nothing.
func TestALateCancelPostsTheAnswerThatStands(t *testing.T) {
	h := startBridge(t, "late-cancel")
	h.promptBuzz(t, firstPrompt(t))
	want := "albatross\n\nThe turn had already finished before the Stop reached fleet, so nothing was stopped: what it did (tool calls included) stands. See http://fleet.test/chat?c=1"
	if body, _ := h.file("buzz-stdin"); body != want {
		t.Errorf("posted %q", body)
	}
}

// A Stop fleet could not confirm may leave the turn running: the asker gets
// fleet's note and where to stop it, not the narration before it.
func TestAnUnconfirmedStopPostsFleetsNote(t *testing.T) {
	h := startBridge(t, "unconfirmed-stop")
	h.promptBuzz(t, firstPrompt(t))
	want := "fleet could not confirm this turn stopped (timeout). It may still be running: stop it at http://fleet.test/chat?c=1"
	if body, _ := h.file("buzz-stdin"); body != want {
		t.Errorf("posted %q", body)
	}
}

// fleet acp refuses session/new for a credential problem, before any prompt.
// Passed on, buzz-acp would retry the mention up to ten times and the asker
// would see nothing (seen live). Instead buzz-acp gets a session, and the
// prompt on it is answered with fleet's reason, without reaching fleet.
func TestARefusedSessionTellsTheAskerWhy(t *testing.T) {
	h := startBridge(t, "refuse-session")
	h.send(1, "initialize", map[string]any{"protocolVersion": 1})
	h.until(1)
	h.send(2, "session/new", map[string]any{"cwd": "/tmp", "mcpServers": []any{}})
	lines := h.until(2)
	res, _ := lines[len(lines)-1]["result"].(map[string]any)
	sid, _ := res["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new answered %v, want a placeholder session", lines[len(lines)-1])
	}
	h.send(3, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": firstPrompt(t)}}})
	lines = h.until(3)
	if r, _ := lines[len(lines)-1]["result"].(map[string]any); r["stopReason"] != "end_turn" {
		t.Errorf("prompt answered %v, want end_turn", lines[len(lines)-1])
	}
	if body, _ := h.file("buzz-stdin"); body != "fleet could not answer: server rejected the request (403): nobody@example.com is not a fleet user" {
		t.Errorf("posted %q", body)
	}
	if _, ok := h.file("agent-prompt"); ok {
		t.Error("the prompt reached the agent that refused the session")
	}
}
