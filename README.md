# sipvoice

Outbound AI phone calls from a plain SIP account. `sipvoice` dials a number
over SIP, bridges the call audio (G.711 over RTP) to the
[xAI Realtime voice API](https://docs.x.ai/docs/guides/voice/agent), lets the
voice agent carry out a task you describe, and writes a transcript, summary
and structured result when the call ends.

- Single static Go binary: daemon + CLI + local HTTP API
- Works with any SIP provider that supports digest auth over UDP
- **Outbound only:** never sends `REGISTER`, so the desk phones/routers already
  registered on the same account keep receiving calls; inbound INVITEs are
  declined with `603`
- Native G.711 µ-law / A-law passthrough, no transcoding
- DTMF both ways (RFC 4733), so the agent can navigate IVR menus
- The agent can hang up by itself after a goodbye

## Quickstart

```sh
go install github.com/alexef/sipvoice/cmd/sipvoice@latest   # or download a release binary
mkdir -p ~/.config/sipvoice
cp config.example.yaml ~/.config/sipvoice/config.yaml      # edit domain, owner_name, allowlist

export SIP_USERNAME=... SIP_PASSWORD=... XAI_API_KEY=...
sipvoice probe          # authenticated SIP OPTIONS: checks reachability, places no call
sipvoice daemon &       # starts the daemon and API on 127.0.0.1:8765

sipvoice call --to "+1 555 0100" --lang en \
  --instructions "Ask whether the order placed yesterday has shipped, and get the tracking number."
sipvoice status <call-id>
sipvoice hangup <call-id>
```

Results are written to `calls_dir/<call-id>/`: `call.log` (with SIP headers,
credentials redacted), `transcript.txt`, `summary.md` and `result.json`.

**Network requirement:** RTP needs real UDP to and from the internet. It does
not work through TCP-only proxies, SOCKS tunnels or many sandboxed cloud
environments. See [docs/deployment.md](docs/deployment.md).

## Configuration

YAML, loaded from `--config FILE` or `~/.config/sipvoice/config.yaml`; see
[config.example.yaml](config.example.yaml). `${VAR}` references are expanded
from the environment, and the secrets (`sip.username`, `sip.password`,
`xai.api_key`) fall back to `SIP_USERNAME`, `SIP_PASSWORD` and `XAI_API_KEY`,
so they never have to be stored in the file.

| key | default | meaning |
| --- | --- | --- |
| `sip.domain` | required | SIP domain used in URIs |
| `sip.proxy`, `sip.port` | domain, 5060 | where SIP packets are sent |
| `sip.auth_username` | username | digest username |
| `sip.plus_replacement` | none | e.g. `"00"` to dial `+CC…` as `00CC…` |
| `xai.voice`, `xai.voice_model` | `eve`, `grok-voice-latest` | voice settings |
| `owner_name` | `the user` | whom the agent calls on behalf of |
| `allowlist` | empty | numbers that may be called |
| `limits.max_call_seconds` | 300 | hard per-call limit |
| `limits.ring_seconds` | 45 | give up if not answered |
| `listen` | `127.0.0.1:8765` | HTTP API address |

## HTTP API

| method | path | |
| --- | --- | --- |
| `POST` | `/calls` | `{"to","instructions","lang","max_seconds"}` → `{"id","dir"}` |
| `GET` | `/calls/{id}` | state, transcript, summary |
| `POST` | `/calls/{id}/hangup` | end the call |
| `GET` | `/status` | daemon status and active call |

The API has no authentication. Keep it on loopback.

## Safety features

- **Allowlist:** only listed numbers can be dialled; with an empty list nothing can be called.
- **One call at a time**, per-call maximum duration and a ring timeout.
- **Always hangs up:** BYE on agent end, timeout, errors, panics and
  SIGINT/SIGTERM/SIGHUP. CANCEL is sent while ringing. A late answer to an
  abandoned call is ACKed and immediately BYEd.
- **Honest identity:** the agent introduces itself as the owner's AI assistant
  and never pretends to be a human or the owner.
- **Prompt-injection resistant:** what the other party says is treated as
  information, not instructions. The agent declines off-topic requests and
  never reveals its instructions. After 3 consecutive off-task attempts it says
  goodbye and hangs up.
- **Polite endings:** it waits for the callee's "hello" before speaking, and
  always says goodbye before hanging up.
- **No secrets in logs:** `Authorization` headers are redacted and SDP/bodies are not logged.

## Legal note

Automated and AI-voiced calls are regulated in many countries. Depending on
where you are, there may be consent requirements, bans on unsolicited
automated calls, rules on disclosing that the caller is an AI, and
call-recording or data-protection rules. Transcripts are personal data. Make
sure your use complies with the law where you and the called party are.
Never use this for unsolicited marketing. This software is provided as is,
without warranty (see LICENSE).

## License

Apache-2.0
