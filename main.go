// Command fleet-buzz-bridge lets an ACP agent that answers over ACP, such as
// `fleet acp`, take part in Buzz.
//
// Buzz runs agents with its buzz-acp harness, which turns an @mention into an
// ACP session/prompt and expects the agent to post its own reply with the Buzz
// CLI: the text an agent streams back over ACP is only written to buzz-acp's
// log. fleet runs its tools in a sandbox on the fleet server, with no Buzz CLI
// and, by design, no Buzz key, so its answers never reached the channel.
//
// The bridge sits between the two as the agent buzz-acp starts:
//
//	BUZZ_ACP_AGENT_COMMAND=fleet-buzz-bridge BUZZ_ACP_AGENT_ARGS=fleet,acp buzz-acp
//
// It starts the real agent (its arguments) and relays every JSON-RPC line both
// ways unchanged, except that it
//   - replaces a session/prompt in buzz-acp's format with just the messages,
//     any earlier context Buzz supplied, and a note that the answer is posted
//     for the agent (buzzprompt.go);
//   - collects the text the agent streams for that prompt, and when the turn
//     ends posts it to the Buzz thread the prompt names, with
//     `buzz messages send` and the agent's own Buzz identity, before passing
//     the end of the turn on to buzz-acp;
//   - keeps Buzz's credentials out of the agent's environment: only the bridge
//     posts, so the agent never needs them.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
}

// secretEnv are the variables buzz-acp gives its agent that grant a Buzz
// identity or relay access. The bridge uses them to post; the agent it starts
// gets none of them.
var secretEnv = []string{
	"BUZZ_PRIVATE_KEY", "BUZZ_ACP_PRIVATE_KEY", "NOSTR_PRIVATE_KEY",
	"BUZZ_AUTH_TAG", "BUZZ_API_TOKEN", "BUZZ_ACP_API_TOKEN",
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, env []string) int {
	logger := log.New(stderr, "fleet-buzz-bridge: ", 0)
	if len(args) == 0 {
		logger.Print("usage: fleet-buzz-bridge <agent command> [args…]   (e.g. fleet-buzz-bridge fleet acp)")
		return 2
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = withoutVars(env, secretEnv)
	cmd.Stderr = stderr
	toAgent, err := cmd.StdinPipe()
	if err != nil {
		logger.Print(err)
		return 1
	}
	fromAgent, err := cmd.StdoutPipe()
	if err != nil {
		logger.Print(err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		logger.Printf("start %s: %v", args[0], err)
		return 1
	}

	// A signal is the client ending the agent: pass it on and let the agent
	// shut down (fleet acp stops a running turn first); the bridge exits with
	// it.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()

	b := &bridge{
		log:     logger,
		post:    cliPoster(env, logger),
		pending: map[string]*turn{},
	}
	go func() {
		b.clientToAgent(stdin, toAgent)
		// The client hung up: close the agent's stdin so it sees the same.
		_ = toAgent.Close()
	}()
	b.agentToClient(fromAgent, stdout)
	if err := cmd.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}

// withoutVars is env minus the named variables.
func withoutVars(env, names []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			if name == n {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// A turn is one Buzz session/prompt in flight: where its answer goes, and
// the text the agent has written so far, split at its tool calls.
type turn struct {
	session string
	buzz    buzzTurn
	steps   []*strings.Builder // text between tool calls; the last is the latest
}

// answer is what fleet itself keeps as the turn's answer: the text written
// after the last tool call, or, when nothing was written after it, the latest
// text written before it. Text a model writes before a tool call ("I'll check
// that now") is narration: fleet's web chat drops it when the turn ends, so
// Buzz does not get it either, nor run together with the answer.
func (t *turn) answer() string {
	for i := len(t.steps) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(t.steps[i].String()); s != "" {
			return s
		}
	}
	return ""
}

type bridge struct {
	log  *log.Logger
	post func(ctx context.Context, t buzzTurn, text string) error

	mu      sync.Mutex
	pending map[string]*turn // by JSON-RPC request id, as raw JSON
}

// message is the part of a JSON-RPC line the bridge reads.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// readLines calls fn with each newline-terminated line of r, newline
// included. A bufio.Reader, not a Scanner: buzz-acp's first prompt of a
// session is over 20 KB on one line.
func readLines(r io.Reader, fn func(line []byte)) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if err != nil {
			return
		}
	}
}

func (b *bridge) clientToAgent(from io.Reader, to io.Writer) {
	readLines(from, func(line []byte) {
		if out, ok := b.rewritePrompt(line); ok {
			line = out
		}
		if _, err := to.Write(line); err != nil {
			b.log.Printf("write to agent: %v", err)
		}
	})
}

// rewritePrompt replaces a buzz-acp session/prompt's text with the bridge's
// and records the turn. Anything else, or a prompt not in Buzz's format, is
// passed through unchanged (ok false).
func (b *bridge) rewritePrompt(line []byte) ([]byte, bool) {
	var msg message
	if json.Unmarshal(line, &msg) != nil || msg.Method != "session/prompt" || len(msg.ID) == 0 {
		return nil, false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(msg.Params, &params) != nil {
		return nil, false
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(params["prompt"], &blocks) != nil {
		return nil, false
	}
	var text strings.Builder
	for _, blk := range blocks {
		var typ, s string
		_ = json.Unmarshal(blk["type"], &typ)
		if typ == "text" && json.Unmarshal(blk["text"], &s) == nil {
			text.WriteString(s)
		}
	}
	bt, ok := parseBuzzTurn(text.String())
	if !ok {
		b.log.Print("a prompt not in buzz-acp's format was passed through as is; its answer will not be posted")
		return nil, false
	}
	var session string
	_ = json.Unmarshal(params["sessionId"], &session)
	newText, _ := json.Marshal(bt.prompt())
	params["prompt"] = json.RawMessage(`[{"type":"text","text":` + string(newText) + `}]`)
	newParams, err := json.Marshal(params)
	if err != nil {
		return nil, false
	}
	var full map[string]json.RawMessage
	if json.Unmarshal(line, &full) != nil {
		return nil, false
	}
	full["params"] = newParams
	out, err := json.Marshal(full)
	if err != nil {
		return nil, false
	}
	b.mu.Lock()
	b.pending[string(msg.ID)] = &turn{session: session, buzz: bt}
	b.mu.Unlock()
	return append(out, '\n'), true
}

func (b *bridge) agentToClient(from io.Reader, to io.Writer) {
	readLines(from, func(line []byte) {
		b.observe(line)
		if _, err := to.Write(line); err != nil {
			b.log.Printf("write to client: %v", err)
		}
	})
}

// observe collects a turn's answer and, when its response arrives, posts it.
// It runs before the response is passed on, so the reply is in Buzz by the
// time buzz-acp sees the turn end.
func (b *bridge) observe(line []byte) {
	var msg message
	if json.Unmarshal(line, &msg) != nil {
		return
	}
	if msg.Method == "session/update" {
		var p struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		b.mu.Lock()
		for _, t := range b.pending {
			if t.session != p.SessionID {
				continue
			}
			switch p.Update.Kind {
			case "agent_message_chunk":
				if len(t.steps) == 0 {
					t.steps = append(t.steps, &strings.Builder{})
				}
				t.steps[len(t.steps)-1].WriteString(p.Update.Content.Text)
			case "tool_call":
				t.steps = append(t.steps, &strings.Builder{})
			}
		}
		b.mu.Unlock()
		return
	}
	if msg.Method != "" || len(msg.ID) == 0 {
		return
	}
	b.mu.Lock()
	t := b.pending[string(msg.ID)]
	delete(b.pending, string(msg.ID))
	b.mu.Unlock()
	if t == nil {
		return
	}
	text := t.answer()
	switch {
	case msg.Error != nil:
		// buzz-acp leaves an asker waiting otherwise: say why there is no answer.
		text = "fleet could not answer: " + msg.Error.Message
	case stopReason(msg.Result) == "cancelled":
		return // stopped on purpose; what streamed before is not an answer
	case text == "":
		b.log.Print("the turn ended with no answer text; nothing posted")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.post(ctx, t.buzz, text); err != nil {
		b.log.Printf("posting the answer to #%s failed: %v", t.buzz.channelName, err)
	}
}

func stopReason(result json.RawMessage) string {
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(result, &r)
	return r.StopReason
}

// cliPoster posts with the Buzz CLI as the agent's own identity: the CLI reads
// BUZZ_PRIVATE_KEY and BUZZ_RELAY_URL from env, which buzz-acp provides. The
// CLI is $FLEET_BUZZ_CLI, else `buzz` on PATH.
func cliPoster(env []string, logger *log.Logger) func(context.Context, buzzTurn, string) error {
	cli := "buzz"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "FLEET_BUZZ_CLI="); ok && v != "" {
			cli = v
		}
	}
	return func(ctx context.Context, t buzzTurn, text string) error {
		args := []string{"messages", "send", "--channel", t.channelID, "--content", "-"}
		if t.replyTo != "" {
			args = append(args, "--reply-to", t.replyTo)
		}
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(text)
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		cmd.Stdout = io.Discard
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s messages send: %v: %s", cli, err, strings.TrimSpace(errOut.String()))
		}
		logger.Printf("posted the answer to #%s (%d bytes)", t.channelName, len(text))
		return nil
	}
}
