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
	if len(bt.events) != 1 || bt.events[0].from != "alice" || bt.events[0].content != "@fleet hey" {
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

func TestBatchedEventsAndAFallbackReplyTarget(t *testing.T) {
	ev := func(id, from, content string) string {
		return "<buzz-event type=\"@mention\">\nEvent ID: " + id + "\nFrom: " + from + " (npub: npub1x, hex: x)\nContent: " + content + "\nTags: []\n</buzz-event>\n"
	}
	// No --reply-to in <context>: thread under the last event.
	text := "<context>\nChannel: general (#" + fixtureChannel + ")\n</context>\n<buzz-events>\n" +
		ev(strings.Repeat("1", 64), "alice", "first") + ev(strings.Repeat("2", 64), "bob", "second") + "</buzz-events>"
	bt, ok := parseBuzzTurn(text)
	if !ok || len(bt.events) != 2 {
		t.Fatalf("ok=%v events=%+v", ok, bt.events)
	}
	if bt.replyTo != strings.Repeat("2", 64) {
		t.Errorf("reply-to = %s, want the last event", bt.replyTo)
	}
	if p := bt.prompt(); !strings.Contains(p, "Messages for you") || !strings.Contains(p, "alice: first\n\nbob: second") {
		t.Errorf("prompt:\n%s", p)
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
