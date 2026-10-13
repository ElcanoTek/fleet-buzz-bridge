# fleet-buzz-bridge

Lets `fleet acp` answer in [Buzz](https://github.com/block/buzz).

Buzz runs agents with its `buzz-acp` harness: an @mention becomes an ACP
`session/prompt`, and the agent is expected to post its own reply with the Buzz
CLI (`buzz messages send`). The text an agent streams back over ACP is only
written to `buzz-acp`'s log (`handle_session_update` in
`crates/buzz-acp/src/acp.rs`). fleet answers over ACP and runs its tools in a
sandbox on the fleet server, with no Buzz CLI and no Buzz key, so with
`buzz-acp` pointed straight at `fleet acp` a turn runs but the answer never
reaches the channel.

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
  reply @mentions whoever asked (by name and with a mention tag; a name
  `buzz-acp` did not give is looked up with `buzz users get`): Buzz agents
  wake on a mention, not on a reply in the thread. After three mentioned
  replies to the same asker in the same thread within ten minutes, replies
  there stop mentioning them, so two agents cannot keep waking each other.
  Messages that reach the agent together, as a batch or as one that arrived
  mid-turn (which `buzz-acp` merges with the request it stopped), get one
  reply, under the last of them, mentioning everyone who asked. An error
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
- **passes on no MCP servers.** `buzz-acp` offers every agent Buzz's own MCP
  server, started with the agent's Buzz key in its environment. fleet never
  uses an MCP server a client sends, and would say so at the top of a reply,
  so `session/new` reaches `fleet acp` with an empty list; stderr names what
  was dropped.
- **keeps Buzz's credentials from fleet.** `BUZZ_PRIVATE_KEY` and the other
  Buzz credential variables are removed from `fleet acp`'s environment; only
  the bridge posts.

A prompt not in `buzz-acp`'s format is passed through unchanged, and its
answer is not posted.

## Use

The bridge needs Go 1.26.8 or later to build, and fleet v2026.10.09.5 or
later: an older `fleet acp` can miss a cancel that `buzz-acp` sends right
behind a prompt, and let that turn run on. With `~/bin` and `fleet` on
`PATH`:

```sh
git clone https://github.com/ElcanoTek/fleet-buzz-bridge
cd fleet-buzz-bridge
go build -o ~/bin/fleet-buzz-bridge .

export BUZZ_PRIVATE_KEY=…            # the agent's own identity, a workspace member
export BUZZ_RELAY_URL=wss://…
export BUZZ_ACP_AGENT_COMMAND=fleet-buzz-bridge
export BUZZ_ACP_AGENT_ARGS=fleet,acp  # the agent the bridge starts, and its args
export FLEET_USER_EMAIL=buzz-bot@example.com
buzz-acp
```

`fleet acp` resolves its server and token as usual (see fleet's
[`docs/ACP.md`](https://github.com/ElcanoTek/fleet/blob/main/docs/ACP.md),
"Buzz", for the whole setup). The bridge posts with `buzz` on `PATH`, or
`$FLEET_BUZZ_CLI`. `buzz-acp` answers only the agent's owner by default; an
agent run from the command line may need `BUZZ_ACP_RESPOND_TO=allowlist` with
`BUZZ_ACP_RESPOND_TO_ALLOWLIST=<hex pubkeys>`.

## Limits

- One reply per turn, posted when the turn ends: nothing streams into Buzz
  while fleet works, and the bridge adds no reactions (beyond `buzz-acp`'s
  own) or edits.
- fleet's own notes in a reply (a queued message, an approval link to the
  fleet web UI) are posted as written.
- It reads `buzz-acp`'s prompt format, checked against block/buzz `70d2ca7`,
  Buzz Desktop 0.5.27 and `buzz-sprig` built from `16eb0b6`. The older
  `buzz-acp` in `buzz-sprig:sha-6530b58`, the image Buzz Desktop 0.5.27 fills
  in for a Kubernetes agent, uses a format it does not read. If the format
  changes, prompts pass through unrewritten and answers are not posted, and
  the bridge says so on stderr.

## fleet-buzz-gateway

`gateway/` is a second, separate command, for running the agent in a
Kubernetes pod on the fleet server (Buzz Desktop's "Run on: kubernetes").
fleet's chat API listens on loopback only and trusts two headers: its shared
server token, which can act as any fleet user, and the user's email. The
gateway lets the pod reach that API without holding the token. It:

- listens on an address only the pods reach (the cluster bridge, e.g.
  `10.42.0.1`), bound with `IP_FREEBIND` so it can start before the cluster
  has created that address;
- forwards to the chat API (`-upstream`, `http://127.0.0.1:8080` by default),
  replacing the token header with the real token, read from `-token-file`,
  and the user header with the one user it runs as (`-email`), and dropping
  the other headers fleet trusts from a proxy;
- forwards only what `fleet acp` calls (`POST /chat`, `GET /me`,
  `POST /conversations/{id}/cancel`, `GET /client-config`, `GET /healthz`)
  and refuses everything else with 403;
- streams responses as they come (a turn is server-sent events).

It builds on Linux only (it uses `IP_FREEBIND`).

```sh
go build -o fleet-buzz-gateway ./gateway
fleet-buzz-gateway -listen 10.42.0.1:8081 -token-file /etc/fleet-buzz/fleet-token -email buzz-bot@example.com
```

The pod then runs `fleet acp --server http://10.42.0.1:8081` with a placeholder
token, which the gateway replaces. Anything that can reach the gateway's
address acts as that user without a token, so keep the cluster to this agent.
The "Run it on Kubernetes from Buzz Desktop" section of fleet's
[`docs/ACP.md`](https://github.com/ElcanoTek/fleet/blob/main/docs/ACP.md) has
the whole setup: k3s, a systemd unit for the gateway, the agent image and Buzz
Desktop's fields.

## Tests

`go test ./...` (on Linux; elsewhere `go test .` covers the bridge) runs the
prompt parser against a real `buzz-acp` prompt (identities replaced), the
bridge end to end against a fake agent and a fake `buzz` CLI, and the gateway
against a fake chat API: the token and user are pinned, only `fleet acp`'s
calls get through, and a turn streams. Checked live on 2026-10-08 with
`buzz-acp` (block/buzz `70d2ca7`), a hosted Buzz relay and `fleet acp` against
a real `fleet serve`: an @mention got fleet's answer as a reply in its thread.
The gateway was checked live on 2026-10-09, with the agent in a k3s pod on a
fleet server.

## License

MIT, see [LICENSE](LICENSE). This covers the bridge and `gateway/`.
`testdata/first-prompt.txt` quotes excerpts of `buzz-acp`'s prompt from
[block/buzz](https://github.com/block/buzz), which is licensed under the
[Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0).
