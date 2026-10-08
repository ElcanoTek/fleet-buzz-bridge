package main

import (
	"os"
	"strings"
	"testing"
)

// testdata/first-prompt.txt is a real buzz-acp prompt (buzz-acp at block/buzz
// 70d2ca7, 2026-10-08) for an @mention, with identities, IDs and most of the
// <base> instructions replaced.
func firstPrompt(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/first-prompt.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	fixtureChannel = "11111111-2222-3333-4444-555555555555"
	fixtureEvent   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParsesARealBuzzPrompt(t *testing.T) {
	bt, ok := parseBuzzTurn(firstPrompt(t))
	if !ok {
		t.Fatal("not recognised as a buzz-acp prompt")
	}
	if bt.channelID != fixtureChannel || bt.channelName != "general" || bt.replyTo != fixtureEvent {
		t.Errorf("target = #%s %s reply-to %s", bt.channelName, bt.channelID, bt.replyTo)
	}
	if len(bt.events) != 1 || bt.events[0].from != "alice" || !bt.events[0].named || bt.events[0].content != "@fleet hey" {
		t.Errorf("events = %+v", bt.events)
	}
	p := bt.prompt()
	for _, want := range []string{"#general", "alice: @fleet hey", "posted to that Buzz thread"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	// Buzz's instructions to post with its CLI must not reach an agent that
	// has no CLI: they are what sent fleet looking for one.
	for _, gone := range []string{"<base>", "buzz messages send", "--reply-to", "npub", "Tags:"} {
		if strings.Contains(p, gone) {
			t.Errorf("prompt still carries %q:\n%s", gone, p)
		}
	}
}

// Later prompts of a session have no <base>, and may carry earlier messages.
func TestKeepsEarlierContextAndMultiLineMessages(t *testing.T) {
	text := "<context>\nChannel: dev (#" + fixtureChannel + ")\nIMPORTANT: reply with `--reply-to " + fixtureEvent + "` on buzz messages send\n</context>\n\n" +
		"<thread-context>\nbob: the build is red\nalice: since when?\n</thread-context>\n\n" +
		"<buzz-event type=\"@mention\">\nEvent ID: " + strings.Repeat("d", 64) + "\nChannel: dev (#" + fixtureChannel + ")\nKind: 9\n" +
		"From: alice (npub: npub1x, hex: " + strings.Repeat("a", 64) + ")\nTime: 2026-10-08T12:00:00+00:00\n" +
		"Content: @fleet look at this:\n\nline one\nline two\nTags: [[\"p\",\"x\"]]\n</buzz-event>"
	bt, ok := parseBuzzTurn(text)
	if !ok {
		t.Fatal("not recognised")
	}
	if bt.replyTo != fixtureEvent {
		t.Errorf("reply-to = %s, want the one <context> names, not the event's own id", bt.replyTo)
	}
	if got := bt.events[0].content; got != "@fleet look at this:\n\nline one\nline two" {
		t.Errorf("content = %q", got)
	}
	p := bt.prompt()
	if !strings.Contains(p, "Earlier in this thread:\nbob: the build is red\nalice: since when?") {
		t.Errorf("thread context lost:\n%s", p)
	}
}

// listed is an event as buzz-acp lists it in a section (format_prompt and
// format_event_block in its queue.rs at 70d2ca7): under an "--- Event N ---"
// heading, with no <buzz-event> around it.
func listed(n int, id, from, hex, content string) string {
	sender := from + " (npub: npub1" + hex[:6] + ", hex: " + hex + ")"
	if strings.HasPrefix(from, "npub1") { // no name for the sender: buzz-acp shows the npub alone
		sender = from + " (hex: " + hex + ")"
	}
	return "--- Event " + string(rune('0'+n)) + " (@mention) ---\nEvent ID: " + id + "\nChannel: general (#" + fixtureChannel + ")\nKind: 9\n" +
		"From: " + sender + "\nTime: 2026-10-08T14:42:19+00:00\n" +
		"Content: " + content + "\nTags: [[\"p\",\"" + strings.Repeat("0", 64) + "\"]]\nParsed: mentions=[fleet (" + strings.Repeat("0", 64) + ")]"
}

var (
	kristianHex = strings.Repeat("8", 64)
	fizzHex     = strings.Repeat("f", 64)
	kristianMsg = strings.Repeat("1", 64)
	fizzMsg     = strings.Repeat("2", 64)
)

// Seen live: a person's mention and an agent's, eight seconds apart, reached
// a just-started agent as one batch, and buzz-acp gave an agent↔agent turn
// at the top level no reply target. The reply must still go under the last
// message and mention both askers.
func TestABatchIsAnsweredUnderItsLastMessage(t *testing.T) {
	text := "<context>\nScope: channel\nChannel: general (#" + fixtureChannel + ")\n</context>\n\n" +
		"<buzz-events count=\"2\">\n" +
		listed(1, kristianMsg, "kristian", kristianHex, "@Fizz ask @fleet with the last Anthropic release") + "\n\n" +
		listed(2, fizzMsg, "Fizz", fizzHex, "@fleet what's the latest Anthropic release?") +
		"\n</buzz-events>"
	bt, ok := parseBuzzTurn(text)
	if !ok || len(bt.events) != 2 {
		t.Fatalf("ok=%v events=%+v", ok, bt.events)
	}
	if bt.replyTo != fizzMsg {
		t.Errorf("reply-to = %s, want the last message", bt.replyTo)
	}
	if bt.events[0].pubkey != kristianHex || bt.events[1].pubkey != fizzHex {
		t.Errorf("senders = %+v", bt.events)
	}
	p := bt.prompt()
	if !strings.Contains(p, "Messages for you in the Buzz channel #general:\n\nkristian: @Fizz ask @fleet with the last Anthropic release\n\nFizz: @fleet what's the latest Anthropic release?") {
		t.Errorf("prompt:\n%s", p)
	}
	for _, gone := range []string{"--- Event", "Tags:", "npub", "Parsed:"} {
		if strings.Contains(p, gone) {
			t.Errorf("prompt still carries %q:\n%s", gone, p)
		}
	}
}

// A message that arrives mid-turn makes buzz-acp stop the turn and send a
// merged one: the request the agent was working on, then the new message.
// The reply answers both askers, since the stopped turn posted nothing.
func TestAMergedTurnKeepsWhatTheAgentWasWorkingOn(t *testing.T) {
	for _, c := range []struct{ earlierTag, newTag, earlierTitle, head, note string }{
		{"what-you-were-working-on", "new-message-arrived-while-you-were-working",
			"What you were working on:", "A message for you in the Buzz channel #general, sent while you were working:", "Carry on with that work"},
		{"previous-request-interrupted-before-completion", "new-request-supersedes-previous",
			"The request you were working on, which was stopped:", "A message for you in the Buzz channel #general, which replaces it:", "Answer the new request."},
	} {
		text := "<context>\nScope: thread\nChannel: general (#" + fixtureChannel + ")\nIMPORTANT: reply with `--reply-to " + fixtureEvent + "`\n</context>\n\n" +
			"<" + c.earlierTag + ">\n" + listed(1, kristianMsg, "kristian", kristianHex, "find the latest release") + "\n</" + c.earlierTag + ">\n\n" +
			"<" + c.newTag + ">\n" + listed(1, fizzMsg, "Fizz", fizzHex, "and its price") + "\n</" + c.newTag + ">\n\n" +
			"Note: buzz-acp's closing note."
		bt, ok := parseBuzzTurn(text)
		if !ok {
			t.Fatalf("%s: not recognised", c.newTag)
		}
		if bt.replyTo != fixtureEvent {
			t.Errorf("%s: reply-to = %s, want the one <context> names", c.newTag, bt.replyTo)
		}
		if len(bt.events) != 1 || bt.events[0].from != "Fizz" || len(bt.earlier) != 1 || bt.earlier[0].from != "kristian" {
			t.Errorf("%s: events = %+v, earlier = %+v", c.newTag, bt.events, bt.earlier)
		}
		p := bt.prompt()
		want := c.earlierTitle + "\n\nkristian: find the latest release\n\n" + c.head + "\n\nFizz: and its price\n\n" + c.note
		if !strings.Contains(p, want) || strings.Contains(p, "--- Event") || strings.Contains(p, "Tags:") {
			t.Errorf("%s: prompt:\n%s\nwant it to contain:\n%s", c.newTag, p, want)
		}
		if a := bt.askers(); len(a) != 2 || a[0].pubkey != kristianHex || a[1].pubkey != fizzHex {
			t.Errorf("%s: askers = %+v", c.newTag, a)
		}
	}
}

func TestLeavesOtherPromptsAlone(t *testing.T) {
	for name, text := range map[string]string{
		"plain text":            "What is 2+2?",
		"context, no channel":   "<context>\nScope: channel\n</context>\n<buzz-event>\nContent: hi\nTags: []\n</buzz-event>",
		"channel, nothing said": "<context>\nChannel: general (#" + fixtureChannel + ")\n</context>",
	} {
		if _, ok := parseBuzzTurn(text); ok {
			t.Errorf("%s: recognised as a buzz-acp prompt", name)
		}
	}
}

// A sender with no profile name is shown by its npub alone, and a file
// attached in Buzz comes with a note that it cannot be opened from fleet.
func TestUnnamedSendersAndAttachedFiles(t *testing.T) {
	text := "<context>\nChannel: general (#" + fixtureChannel + ")\n</context>\n<buzz-event type=\"@mention\">\nEvent ID: " + fixtureEvent +
		"\nFrom: npub1tester (hex: " + strings.Repeat("d", 64) + ")\nContent: @fleet What colour is this?\n![image](https://relay.example/media/abc.png)\nTags: []\n</buzz-event>"
	bt, ok := parseBuzzTurn(text)
	if !ok {
		t.Fatal("not recognised")
	}
	if bt.events[0].from != "npub1tester" || bt.events[0].named {
		t.Errorf("from = %q, named = %v", bt.events[0].from, bt.events[0].named)
	}
	p := bt.prompt()
	if !strings.Contains(p, "need a Buzz login, so they cannot be opened from here") {
		t.Errorf("no note about the attached file:\n%s", p)
	}
	if strings.Contains(firstPromptTurn(t).prompt(), "Buzz login") {
		t.Error("the note appears without an attachment")
	}
}

func firstPromptTurn(t *testing.T) buzzTurn {
	bt, ok := parseBuzzTurn(firstPrompt(t))
	if !ok {
		t.Fatal("fixture not recognised")
	}
	return bt
}
