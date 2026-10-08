# fleet-buzz-bridge

Lets `fleet acp` answer in [Buzz](https://github.com/block/buzz).

Buzz runs agents with its `buzz-acp` harness: an @mention becomes an ACP
`session/prompt`, and the agent is expected to post its own reply with the Buzz
CLI (`buzz messages send`). The text an agent streams back over ACP is only
written to `buzz-acp`'s log (`handle_session_update` in `crates/buzz-acp/src/acp.rs`).
fleet answers over ACP and runs its tools in a sandbox on the fleet server,
with no Buzz CLI and no Buzz key, so with `buzz-acp` pointed straight at
`fleet acp` a turn runs but the answer never reaches the channel.

The bridge is the agent `buzz-acp` starts. It starts `fleet acp` itself and
relays every JSON-RPC line both ways, except that it:

- **rewrites Buzz's prompt.** A `session/prompt` in `buzz-acp`'s format is
  replaced by the message(s), any earlier thread or conversation context Buzz
  supplied, and a note that the reply is posted for the agent. Buzz's standing
  instructions (`<base>`, about 20 KB on a session's first prompt) are left
  out: they tell the agent to use a CLI it does not have.
- **posts the answer.** It collects the `agent_message_chunk` text of that
  prompt and, when the turn ends, posts fleet's answer, the text written after
  the last tool call (as fleet's web chat keeps it; narration before a tool
  call is left out), with
  `buzz messages send --channel <channel> --reply-to <event> --content -` to
  the thread `buzz-acp` named, before passing the end of the turn on. The
  reply @mentions whoever asked (by name and with a mention tag): Buzz agents
  wake on a mention, not on a reply in the thread. After three mentioned
  replies to the same asker in the same thread within ten minutes, replies
  there stop mentioning them, so two agents cannot keep waking each other. An error
  is posted as `fleet could not answer: <reason>`, and `buzz-acp` is told the
  turn ended: it retries a failed prompt up to ten times, and each retry would
  be a new fleet turn and a repeated reply. A cancelled turn posts
  nothing, unless fleet says the Stop came after the turn had finished (its
  answer and that note are posted) or that it could not confirm the Stop (that
  note is posted); a turn with no answer text posts nothing.
- **answers a refused session.** When `fleet acp` refuses `session/new` (a
  wrong token, a user that is not in fleet, a viewer), `buzz-acp` would retry
  the mention up to ten times and the asker would see nothing. The bridge
  gives `buzz-acp` a placeholder session instead, and answers its prompt by
  posting `fleet could not answer: <fleet's reason>` to the thread; the prompt
  never reaches fleet.
- **keeps Buzz's credentials from fleet.** `BUZZ_PRIVATE_KEY` and the other
  Buzz credential variables are removed from `fleet acp`'s environment; only
  the bridge posts.

A prompt not in `buzz-acp`'s format is passed through unchanged, and its
answer is not posted.

## Use

```sh
go build -o ~/bin/fleet-buzz-bridge .

export BUZZ_PRIVATE_KEY=…            # the agent's own identity, a workspace member
export BUZZ_RELAY_URL=wss://…
export BUZZ_ACP_AGENT_COMMAND=fleet-buzz-bridge
export BUZZ_ACP_AGENT_ARGS=fleet,acp  # the agent the bridge starts, and its args
export FLEET_USER_EMAIL=acp-bot@example.com
buzz-acp
```

`fleet acp` resolves its server and token as usual (see fleet's
`docs/ACP.md`). The bridge posts with `buzz` on `PATH`, or `$FLEET_BUZZ_CLI`.
`buzz-acp` answers only the agent's owner by default; an agent run from the
command line may need `BUZZ_ACP_RESPOND_TO=allowlist` with
`BUZZ_ACP_RESPOND_TO_ALLOWLIST=<hex pubkeys>`.

## Limits

- One reply per turn, posted when the turn ends: nothing streams into Buzz
  while fleet works, and there are no reactions or edits.
- fleet's own notes in a reply (a queued message, an approval link to the
  fleet web UI) are posted as written.
- It reads `buzz-acp`'s prompt format (checked against block/buzz `70d2ca7`,
  2026-10-08). If that changes, prompts pass through unrewritten and answers
  are not posted, and the bridge says so on stderr.

## Tests

`go test ./...` runs the prompt parser against a real `buzz-acp` prompt
(identities replaced) and the bridge end to end against a fake agent and a fake
`buzz` CLI. Checked live on 2026-10-08 with `buzz-acp` (block/buzz `70d2ca7`), a
hosted Buzz relay and `fleet acp` against a real `fleet serve`: an @mention got
fleet's answer as a reply in its thread.
