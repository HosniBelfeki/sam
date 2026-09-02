# chat-a2a

A Gemini-backed A2A agent plus a tiny chat REPL, exercising the mesh's a2a
support end to end: agent-card fetch with caller-side rewrite, `message/send`
routing over libp2p, and `contextId` continuity across turns.

## 1. Set your Gemini key

Edit `Dockerfile` and replace `<API_KEY>` in `ENV GEMINI_API_KEY=<API_KEY>`
(same pattern as `gemini-buddy-mcp`). Optionally override the model with
`GEMINI_MODEL` (default `models/gemini-3.5-flash-lite`).

## 2. Host the agent on a mesh node

Assign it in `development/kind/mesh-config.yaml`:

```yaml
node-a:
node-b: chat-a2a
```

Then bring the mesh up:

```sh
make build
make kind-up
```

## 3. Enroll a local caller node

```sh
./development/kind/run-local-node.sh
```

Sidecar API lands on `127.0.0.1:9099` with token `devtoken`.

## 4. Find the provider peer

```sh
./bin/mcp-client -url http://127.0.0.1:9099/mcp -token devtoken \
  -tool discover_remote_services -args '{"type":"a2a","name":"chat"}'
```

Note the peer ID and export it: `export PEER=<peer-id>`. Discovery is
gossip-fed; retry for a few seconds after startup if the list comes back empty.

## 5. See the card rewrite

```sh
curl -s -H 'X-Sam-Authentication: Bearer devtoken' \
  "http://127.0.0.1:9099/sam/$PEER/a2a/chat/.well-known/agent-card.json" | jq
```

The interface URLs point back at this mesh path (not the agent's own address)
and `capabilities.streaming` is forced to `false` — that rewrite is what lets
a stock A2A client work against the mesh unmodified.

## 6. Chat

Requires [`uv`](https://docs.astral.sh/uv/).

```sh
cd development/examples/chat-a2a
uv run --with-requirements requirements.txt chat.py "http://127.0.0.1:9099/sam/$PEER/a2a/chat"
```

Tell the agent your name, then ask for it back a couple of turns later: the
client carries the `contextId` the server minted on the first reply, and the
agent keeps one Gemini chat session per context, so the answer proves the
conversation survived the mesh hop. Each turn is still its own short-lived
A2A task — `taskId` changes every turn, `contextId` is what persists.
