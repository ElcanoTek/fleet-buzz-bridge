package main

import (
	"regexp"
	"strings"
)

// A Buzz turn as buzz-acp hands it to an agent: one session/prompt whose text
// is Buzz's standing instructions (<base>, first prompt of a session only),
// the turn's <context> and one or more <buzz-event> blocks. buzz-acp expects
// the agent to post its own reply with the Buzz CLI; an agent that answers
// over ACP instead has its answer logged and dropped. The bridge therefore
// needs only two things from the prompt: what was said, and where a reply
// goes.
type buzzTurn struct {
	channelID   string // the channel's UUID, for buzz messages send --channel
	channelName string
	replyTo     string // event ID the reply threads under (--reply-to)
	events      []buzzEvent
	sections    []buzzSection // earlier messages and merged requests, verbatim
}

type buzzEvent struct {
	from    string
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
	eventBlock   = regexp.MustCompile(`(?s)<buzz-event[^>]*>\n(.*?)\n</buzz-event>`)
	eventIDLine  = regexp.MustCompile(`(?m)^Event ID: ([0-9a-f]{64})$`)
	fromLine     = regexp.MustCompile(`(?m)^From: (.*)$`)
	// Content runs from its label to the next field buzz-acp writes after it
	// (format_event_block in buzz-acp's queue.rs), so a multi-line message
	// stays whole.
	contentField = regexp.MustCompile(`(?s)\nContent: (.*?)(?:\n(?:Tags|Parsed|Edit of|Reply to|Thread): |\z)`)
)

// The sections that carry what was said before, or a request merged into one
// already in flight. Go's regexp has no back-references, so each tag gets its
// own pattern.
var sectionTags = []string{
	"thread-context",
	"conversation-context",
	"new-message-arrived-while-you-were-working",
	"new-request-supersedes-previous",
}

var sectionBlocks = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, len(sectionTags))
	for _, tag := range sectionTags {
		m[tag] = regexp.MustCompile(`(?s)<` + tag + `[^>]*>\n?(.*?)\n?</` + tag + `>`)
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
	if m := replyToFlag.FindStringSubmatch(ctx[1]); m != nil {
		t.replyTo = m[1]
	}
	for _, block := range eventBlock.FindAllStringSubmatch(text, -1) {
		ev := buzzEvent{from: "someone"}
		if m := fromLine.FindStringSubmatch(block[1]); m != nil {
			ev.from = displayName(m[1])
		}
		if m := contentField.FindStringSubmatch("\n" + block[1]); m != nil {
			ev.content = strings.TrimSpace(m[1])
		}
		if ev.content == "" {
			continue
		}
		t.events = append(t.events, ev)
		// Without a reply target in <context>, thread under the last event:
		// that is the message being answered.
		if m := eventIDLine.FindStringSubmatch(block[1]); m != nil && !replyToFlag.MatchString(ctx[1]) {
			t.replyTo = m[1]
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
	"thread-context":                             "Earlier in this thread",
	"conversation-context":                       "Earlier in this conversation",
	"new-message-arrived-while-you-were-working": "Sent while you were working",
	"new-request-supersedes-previous":            "This replaces the earlier request",
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
	if len(t.events) == 1 {
		b.WriteString("A message for you in the Buzz channel #" + t.channelName + ":\n\n")
	} else if len(t.events) > 1 {
		b.WriteString("Messages for you in the Buzz channel #" + t.channelName + ":\n\n")
	}
	for _, ev := range t.events {
		b.WriteString(ev.from + ": " + ev.content + "\n\n")
	}
	for _, ev := range t.events {
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
