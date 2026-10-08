package main

import (
	"regexp"
	"strings"
)

// A Buzz turn as buzz-acp hands it to an agent: one session/prompt whose text
// is Buzz's standing instructions (<base>, first prompt of a session only),
// the turn's <context> and the event(s) it is about. buzz-acp expects the
// agent to post its own reply with the Buzz CLI; an agent that answers over
// ACP instead has its answer logged and dropped. The bridge therefore needs
// only two things from the prompt: what was said, and where a reply goes.
type buzzTurn struct {
	channelID   string // the channel's UUID, for buzz messages send --channel
	channelName string
	replyTo     string      // event ID the reply threads under (--reply-to)
	events      []buzzEvent // the message(s) this turn answers
	// A turn buzz-acp merged because a message arrived while the agent was
	// working: the request it was working on, and the section the new
	// message came in (which says whether it replaces that request).
	earlier  []buzzEvent
	merged   string
	sections []buzzSection // earlier messages in the thread or DM, verbatim
}

type buzzEvent struct {
	from    string // display name
	pubkey  string // hex, for mentioning the sender in the reply
	content string
}

type buzzSection struct {
	tag  string
	body string
}

var (
	contextBlock = regexp.MustCompile(`(?s)<context>\n(.*?)\n</context>`)
	channelLine  = regexp.MustCompile(`(?m)^Channel: (.*?) \(#([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\)$`)
	replyToFlag  = regexp.MustCompile("--reply-to ([0-9a-f]{64})")
	eventBlock   = regexp.MustCompile(`(?s)<buzz-event(?:\s[^>]*)?>\n(.*?)\n</buzz-event>`)
	// Where buzz-acp lists several events in one section, each is headed
	// "--- Event N (<type>) ---" instead of being wrapped in <buzz-event>
	// (format_prompt in buzz-acp's queue.rs).
	listedEvent = regexp.MustCompile(`(?m)^--- Event \d+ \([^)\n]*\) ---$`)
	eventIDLine = regexp.MustCompile(`(?m)^Event ID: ([0-9a-f]{64})$`)
	fromLine    = regexp.MustCompile(`(?m)^From: (.*)$`)
	fromHex     = regexp.MustCompile(`hex: ([0-9a-f]{64})\)`)
	// Content runs from its label to the next field buzz-acp writes after it
	// (format_event_block in buzz-acp's queue.rs), so a multi-line message
	// stays whole.
	contentField = regexp.MustCompile(`(?s)\nContent: (.*?)(?:\n(?:Tags|Parsed|Edit of|Reply to|Thread): |\z)`)
)

// The sections that carry what was said before in the thread or DM, passed
// on as they are.
var sectionTags = []string{
	"thread-context",
	"conversation-context",
}

// The sections that list events. A batch (messages that arrived together) and
// the new message(s) of a merged turn are what the turn answers; a merged
// turn first lists the request the agent was working on, in its own section.
// Seen live: a mention from a person and one from an agent, eight seconds
// apart, reached a just-started agent as one <buzz-events> batch.
var (
	answeredListTags = []string{
		"buzz-events",
		"new-message-arrived-while-you-were-working",
		"new-request-supersedes-previous",
	}
	earlierListTags = []string{
		"what-you-were-working-on",
		"previous-request-interrupted-before-completion",
	}
)

// Go's regexp has no back-references, so each tag gets its own pattern.
var sectionBlocks = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, tags := range [][]string{sectionTags, answeredListTags, earlierListTags} {
		for _, tag := range tags {
			m[tag] = regexp.MustCompile(`(?s)<` + tag + `(?:\s[^>]*)?>\n?(.*?)\n?</` + tag + `>`)
		}
	}
	return m
}()

// parseBuzzTurn reads a buzz-acp prompt. ok is false when the text is not one
// (no channel in a <context> block, or nothing said): the bridge then passes
// the prompt through unchanged and posts nothing, rather than guess.
func parseBuzzTurn(text string) (buzzTurn, bool) {
	ctx := contextBlock.FindStringSubmatch(text)
	if ctx == nil {
		return buzzTurn{}, false
	}
	ch := channelLine.FindStringSubmatch(ctx[1])
	if ch == nil {
		return buzzTurn{}, false
	}
	t := buzzTurn{channelName: ch[1], channelID: ch[2]}
	contextReplyTo := replyToFlag.FindStringSubmatch(ctx[1])
	if contextReplyTo != nil {
		t.replyTo = contextReplyTo[1]
	}
	answered := func(block string) {
		ev, id := parseEvent(block)
		if ev.content == "" {
			return
		}
		t.events = append(t.events, ev)
		// Without a reply target in <context>, thread under the last event:
		// that is the message being answered.
		if id != "" && contextReplyTo == nil {
			t.replyTo = id
		}
	}
	for _, block := range eventBlock.FindAllStringSubmatch(text, -1) {
		answered(block[1])
	}
	for _, tag := range answeredListTags {
		for _, m := range sectionBlocks[tag].FindAllStringSubmatch(text, -1) {
			for _, block := range listedEvents(m[1]) {
				answered(block)
			}
			if tag != "buzz-events" {
				t.merged = tag
			}
		}
	}
	for _, tag := range earlierListTags {
		for _, m := range sectionBlocks[tag].FindAllStringSubmatch(text, -1) {
			for _, block := range listedEvents(m[1]) {
				if ev, _ := parseEvent(block); ev.content != "" {
					t.earlier = append(t.earlier, ev)
				}
			}
		}
	}
	for _, tag := range sectionTags {
		for _, m := range sectionBlocks[tag].FindAllStringSubmatch(text, -1) {
			if body := strings.TrimSpace(m[1]); body != "" {
				t.sections = append(t.sections, buzzSection{tag: tag, body: body})
			}
		}
	}
	if len(t.events) == 0 && len(t.sections) == 0 {
		return buzzTurn{}, false
	}
	return t, true
}

// listedEvents splits a section's "--- Event N (<type>) ---" list into the
// event blocks under each heading.
func listedEvents(body string) []string {
	heads := listedEvent.FindAllStringIndex(body, -1)
	blocks := make([]string, 0, len(heads))
	for i, h := range heads {
		end := len(body)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		blocks = append(blocks, strings.TrimSpace(body[h[1]:end]))
	}
	return blocks
}

// parseEvent reads one event block (format_event_block in buzz-acp's
// queue.rs): who sent it, what it says, and its ID.
func parseEvent(block string) (ev buzzEvent, id string) {
	ev.from = "someone"
	if m := fromLine.FindStringSubmatch(block); m != nil {
		ev.from = displayName(m[1])
		if h := fromHex.FindStringSubmatch(m[1]); h != nil {
			ev.pubkey = h[1]
		}
	}
	if m := contentField.FindStringSubmatch("\n" + block); m != nil {
		ev.content = strings.TrimSpace(m[1])
	}
	if m := eventIDLine.FindStringSubmatch(block); m != nil {
		id = m[1]
	}
	return ev, id
}

// askers are everyone the reply answers: in a merged turn, whoever asked
// what the agent was working on too, since the stopped turn posted nothing.
func (t buzzTurn) askers() []buzzEvent {
	return append(append([]buzzEvent(nil), t.earlier...), t.events...)
}

// displayName is the From field's name, without the keys buzz-acp appends:
// "kristian (npub: …, hex: …)" is "kristian", and a sender with no profile
// name, "npub1… (hex: …)", is its npub.
func displayName(from string) string {
	for _, sep := range []string{" (npub: ", " (hex: "} {
		if i := strings.Index(from, sep); i > 0 {
			from = from[:i]
		}
	}
	return strings.TrimSpace(from)
}

// buzzMedia matches a file attached in Buzz, which buzz-acp passes as a
// Markdown link to the relay's /media/ path.
var buzzMedia = regexp.MustCompile(`\]\(https?://[^)\s]+/media/[^)\s]+\)`)

var sectionTitles = map[string]string{
	"thread-context":       "Earlier in this thread",
	"conversation-context": "Earlier in this conversation",
}

// How a merged turn is put to the agent, after buzz-acp's own framing
// (MergeFraming in its queue.rs): a message that arrived mid-turn is woven
// into the work, a new request replaces it.
var mergedFraming = map[string]struct{ earlier, answered, note string }{
	"new-message-arrived-while-you-were-working": {
		earlier:  "What you were working on",
		answered: "sent while you were working",
		note:     "Carry on with that work, and take the new message into account if it is relevant.",
	},
	"new-request-supersedes-previous": {
		earlier:  "The request you were working on, which was stopped",
		answered: "which replaces it",
		note:     "Answer the new request.",
	},
}

// prompt is what the agent is asked instead of buzz-acp's prompt: the
// messages, any earlier context Buzz supplied, and how the answer is used.
// Buzz's standing instructions (<base>) are left out: they tell the agent to
// post with the Buzz CLI, which an agent answering over ACP does not have,
// and they are about 20 KB on every new session.
func (t buzzTurn) prompt() string {
	var b strings.Builder
	for _, s := range t.sections {
		b.WriteString(sectionTitles[s.tag] + ":\n" + s.body + "\n\n")
	}
	framing, merged := mergedFraming[t.merged]
	if merged && len(t.earlier) > 0 {
		b.WriteString(framing.earlier + ":\n\n")
		for _, ev := range t.earlier {
			b.WriteString(ev.from + ": " + ev.content + "\n\n")
		}
	}
	if len(t.events) > 0 {
		head := "A message for you in the Buzz channel #" + t.channelName
		if len(t.events) > 1 {
			head = "Messages for you in the Buzz channel #" + t.channelName
		}
		if merged {
			head += ", " + framing.answered
		}
		b.WriteString(head + ":\n\n")
	}
	for _, ev := range t.events {
		b.WriteString(ev.from + ": " + ev.content + "\n\n")
	}
	if merged {
		b.WriteString(framing.note + "\n\n")
	}
	for _, ev := range t.askers() {
		if buzzMedia.MatchString(ev.content) {
			// Seen live: the relay answers 401 without a Buzz login, so a
			// fetch fails and the model guessed ("Unknown").
			b.WriteString("Files attached in Buzz (the /media/ links above) need a Buzz login, so they cannot be opened from here: if the message depends on one, say so.\n\n")
			break
		}
	}
	b.WriteString("Your reply is posted to that Buzz thread as you write it, so answer with the message itself. There is no Buzz CLI here, and nothing else is needed to send it.")
	return b.String()
}
