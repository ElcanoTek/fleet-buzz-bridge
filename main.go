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
	"slices"
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
		log:        logger,
		post:       cliPoster(env, logger),
		names:      cliNames(env),
		client:     stdout,
		pending:    map[string]*turn{},
		sessionNew: map[string]bool{},
		refused:    map[string]string{},
		mentioned:  map[string][]time.Time{},
		knownNames: map[string]string{},
		now:        time.Now,
	}
	go func() {
		b.clientToAgent(stdin, toAgent)
		// The client hung up: close the agent's stdin so it sees the same.
		_ = toAgent.Close()
	}()
	b.agentToClient(fromAgent)
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

// fleet acp's notes on a cancelled prompt (internal/acp/agent.go): the Stop
// reached fleet after the turn had finished, or fleet did not confirm it.
const (
	alreadyFinishedNote = "The turn had already finished before the Stop reached fleet"
	notConfirmedNote    = "fleet could not confirm this turn stopped"
)

// allText is everything the agent wrote in the turn, in order.
func (t *turn) allText() string {
	var b strings.Builder
	for _, s := range t.steps {
		b.WriteString(s.String())
	}
	return b.String()
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
	log   *log.Logger
	post  func(ctx context.Context, t buzzTurn, text string, mentions []string) error
	names func(ctx context.Context, pubkeys []string) (map[string]string, error) // display name by pubkey
	now   func() time.Time

	outMu  sync.Mutex // the bridge answers some requests itself, besides relaying
	client io.Writer  // buzz-acp

	mu         sync.Mutex
	pending    map[string]*turn  // by JSON-RPC request id, as raw JSON
	sessionNew map[string]bool   // session/new request ids awaiting the agent's answer
	refused    map[string]string // placeholder session id → why the agent refused it
	nRefused   int
	mentioned  map[string][]time.Time // thread|asker → when the bridge last mentioned them
	knownNames map[string]string      // pubkey → name looked up for an asker buzz-acp gave no name
}

// A reply mentions whoever asked: Buzz agents (and buzz-acp) wake on a
// mention, not on a reply in the thread, so without one an agent that asked
// fleet something never sees the answer (seen live). Two agents that mention
// each other could keep waking each other, so after maxMentions mentioned
// replies to the same asker in the same thread within mentionWindow, replies
// there still go out but no longer mention them.
const (
	maxMentions   = 3
	mentionWindow = 10 * time.Minute
)

// address puts an @mention of each distinct asker in front of text and
// returns their pubkeys, for the post's mention tags.
func (b *bridge) address(ctx context.Context, t buzzTurn, text string) (string, []string) {
	b.lookUpNames(ctx, t)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var names, keys []string
	seen := map[string]bool{}
	for _, ev := range t.askers() {
		if ev.pubkey == "" || seen[ev.pubkey] {
			continue
		}
		seen[ev.pubkey] = true
		name := ev.from
		if n := b.knownNames[ev.pubkey]; !ev.named && n != "" {
			name = n
		}
		key := t.replyTo + "|" + ev.pubkey
		var recent []time.Time
		for _, at := range b.mentioned[key] {
			if now.Sub(at) < mentionWindow {
				recent = append(recent, at)
			}
		}
		if len(recent) >= maxMentions {
			b.mentioned[key] = recent
			b.log.Printf("not mentioning %s again in this thread: %d mentioned replies in %s", name, len(recent), mentionWindow)
			continue
		}
		b.mentioned[key] = append(recent, now)
		names = append(names, "@"+name)
		keys = append(keys, ev.pubkey)
	}
	if len(names) == 0 {
		return text, nil
	}
	return strings.Join(names, " ") + " " + text, keys
}

// nameLookupTimeout bounds looking up askers' names, which comes out of the
// time a reply has to be posted.
const nameLookupTimeout = 10 * time.Second

// lookUpNames finds the profile name of each asker buzz-acp gave only a key
// for, so a reply mentions them by name and not as "@npub1…". Seen live:
// buzz-acp looks up names for a turn's new message, the keys it mentions and
// the thread, but not for the sender of the request it stopped to merge a new
// message in, so that asker came as "npub1… (hex: …)". A name found is kept
// for the bridge's lifetime; on a failed lookup the npub stays.
func (b *bridge) lookUpNames(ctx context.Context, t buzzTurn) {
	if b.names == nil {
		return
	}
	var missing []string
	b.mu.Lock()
	for _, ev := range t.askers() {
		if !ev.named && ev.pubkey != "" && b.knownNames[ev.pubkey] == "" && !slices.Contains(missing, ev.pubkey) {
			missing = append(missing, ev.pubkey)
		}
	}
	b.mu.Unlock()
	if len(missing) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, nameLookupTimeout)
	defer cancel()
	found, err := b.names(ctx, missing)
	if err != nil {
		b.log.Printf("looking up the name of %d asker(s) failed, so they are mentioned by npub: %v", len(missing), err)
		return
	}
	b.mu.Lock()
	for k, n := range found {
		b.knownNames[k] = n
	}
	b.mu.Unlock()
}

func (b *bridge) toClient(line []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	if _, err := b.client.Write(line); err != nil {
		b.log.Printf("write to client: %v", err)
	}
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
		if b.handleRefusedSession(line) {
			return
		}
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
	bt, ok := parseBuzzTurn(promptText(params["prompt"]))
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

func (b *bridge) agentToClient(from io.Reader) {
	readLines(from, func(line []byte) {
		if out := b.observe(line); out != nil {
			line = out
		}
		b.toClient(line)
	})
}

// A session the agent refused to open (fleet acp checks the user and token at
// session/new) gets a placeholder session instead of the error: buzz-acp
// would otherwise retry the mention up to ten times and the asker would see
// nothing, since there is no prompt yet to answer. Prompts on the
// placeholder are answered here, with the agent's reason posted to the
// thread, and never reach the agent.
func (b *bridge) handleRefusedSession(line []byte) bool {
	var msg message
	if json.Unmarshal(line, &msg) != nil {
		return false
	}
	if msg.Method == "session/new" && len(msg.ID) > 0 {
		b.mu.Lock()
		b.sessionNew[string(msg.ID)] = true
		b.mu.Unlock()
		return false
	}
	var p struct {
		SessionID string          `json:"sessionId"`
		Prompt    json.RawMessage `json:"prompt"`
	}
	if msg.Method == "" || json.Unmarshal(msg.Params, &p) != nil {
		return false
	}
	b.mu.Lock()
	reason, refused := b.refused[p.SessionID]
	b.mu.Unlock()
	if !refused {
		return false
	}
	switch msg.Method {
	case "session/prompt":
		bt, ok := parseBuzzTurn(promptText(p.Prompt))
		if !ok {
			b.toClient([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"error":{"code":-32000,"message":` + jsonString(reason) + `}}` + "\n"))
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		text, mentions := b.address(ctx, bt, "fleet could not answer: "+reason)
		if err := b.post(ctx, bt, text, mentions); err != nil {
			b.log.Printf("posting the agent's refusal to #%s failed: %v", bt.channelName, err)
		}
		b.toClient([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{"stopReason":"end_turn"}}` + "\n"))
	case "session/close":
		if len(msg.ID) > 0 {
			b.toClient([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{}}` + "\n"))
		}
	}
	return true // session/cancel and anything else on the placeholder is dropped
}

// promptText joins the text blocks of an ACP prompt.
func promptText(raw json.RawMessage) string {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var text strings.Builder
	for _, blk := range blocks {
		var typ, s string
		_ = json.Unmarshal(blk["type"], &typ)
		if typ == "text" && json.Unmarshal(blk["text"], &s) == nil {
			text.WriteString(s)
		}
	}
	return text.String()
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// observe collects a turn's answer and, when its response arrives, posts it.
// It runs before the response is passed on, so the reply is in Buzz by the
// time buzz-acp sees the turn end. It returns a replacement for line, or nil
// to pass it on as is.
func (b *bridge) observe(line []byte) []byte {
	var msg message
	if json.Unmarshal(line, &msg) != nil {
		return nil
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
			return nil
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
		return nil
	}
	if msg.Method != "" || len(msg.ID) == 0 {
		return nil
	}
	b.mu.Lock()
	if b.sessionNew[string(msg.ID)] {
		delete(b.sessionNew, string(msg.ID))
		if msg.Error == nil {
			b.mu.Unlock()
			return nil
		}
		b.nRefused++
		placeholder := fmt.Sprintf("fleet-buzz-bridge-refused-%d", b.nRefused)
		b.refused[placeholder] = msg.Error.Message
		b.mu.Unlock()
		b.log.Printf("the agent refused a session (%s); its prompts will be answered with that reason", msg.Error.Message)
		return []byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{"sessionId":` + jsonString(placeholder) + `}}` + "\n")
	}
	t := b.pending[string(msg.ID)]
	delete(b.pending, string(msg.ID))
	b.mu.Unlock()
	if t == nil {
		return nil
	}
	text := t.answer()
	var out []byte
	switch {
	case msg.Error != nil:
		// buzz-acp leaves an asker waiting otherwise: say why there is no
		// answer. And tell buzz-acp the turn ended normally: it retries a
		// prompt that fails, up to ten times with backoff, and each retry is a
		// new fleet turn that may repeat what the failed one did (tools
		// included) and posts the same error again. The asker can ask again.
		text = "fleet could not answer: " + msg.Error.Message
		out = []byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{"stopReason":"end_turn"}}` + "\n")
	case stopReason(msg.Result) == "cancelled":
		// Stopped on purpose: what streamed before is not an answer, unless
		// fleet's note says the Stop came too late or was not confirmed.
		// Then the asker needs to know, as they would in an editor.
		full := t.allText()
		switch {
		case strings.Contains(full, alreadyFinishedNote):
			// The turn finished before the Stop: its answer stands, and
			// fleet's note (in the same text) says so.
		case strings.Contains(full, notConfirmedNote):
			text = strings.TrimSpace(full[strings.Index(full, notConfirmedNote):])
		default:
			return nil
		}
	case text == "":
		b.log.Print("the turn ended with no answer text; nothing posted")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	text, mentions := b.address(ctx, t.buzz, text)
	if err := b.post(ctx, t.buzz, text, mentions); err != nil {
		b.log.Printf("posting the answer to #%s failed: %v", t.buzz.channelName, err)
	}
	if out != nil {
		b.log.Printf("fleet's error was posted; buzz-acp is told the turn ended, so it does not retry it")
	}
	return out
}

func stopReason(result json.RawMessage) string {
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(result, &r)
	return r.StopReason
}

// buzzCLI is the Buzz CLI the bridge runs: $FLEET_BUZZ_CLI, else `buzz` on
// PATH. It runs as the agent's own identity: it reads BUZZ_PRIVATE_KEY and
// BUZZ_RELAY_URL from env, which buzz-acp provides.
func buzzCLI(env []string) string {
	cli := "buzz"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "FLEET_BUZZ_CLI="); ok && v != "" {
			cli = v
		}
	}
	return cli
}

// cliNames looks up profile names with `buzz users get`. A key with no name
// in its profile is left out of the result.
func cliNames(env []string) func(context.Context, []string) (map[string]string, error) {
	cli := buzzCLI(env)
	return func(ctx context.Context, pubkeys []string) (map[string]string, error) {
		args := []string{"users", "get"}
		for _, k := range pubkeys {
			args = append(args, "--pubkey", k)
		}
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = env
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%s users get: %v: %s", cli, err, strings.TrimSpace(errOut.String()))
		}
		var profiles []struct {
			Pubkey      string `json:"pubkey"`
			DisplayName string `json:"display_name"`
			Name        string `json:"name"`
		}
		if err := json.Unmarshal(out.Bytes(), &profiles); err != nil {
			return nil, fmt.Errorf("%s users get: %v", cli, err)
		}
		names := map[string]string{}
		for _, p := range profiles {
			n := p.DisplayName
			if strings.TrimSpace(n) == "" {
				n = p.Name
			}
			// One line, as buzz-acp shows a name: it goes in front of the reply.
			if n = strings.Join(strings.Fields(n), " "); n != "" {
				names[strings.ToLower(p.Pubkey)] = n
			}
		}
		return names, nil
	}
}

// cliPoster posts with the Buzz CLI.
func cliPoster(env []string, logger *log.Logger) func(context.Context, buzzTurn, string, []string) error {
	cli := buzzCLI(env)
	return func(ctx context.Context, t buzzTurn, text string, mentions []string) error {
		args := []string{"messages", "send", "--channel", t.channelID, "--content", "-"}
		if t.replyTo != "" {
			args = append(args, "--reply-to", t.replyTo)
		}
		for _, k := range mentions {
			args = append(args, "--mention", k)
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
