---
title: "Your own mesh"
linkTitle: "Your own mesh"
weight: 2
aliases:
  - /docs/user/device-enrollment/
---

The quick start joined a mesh that someone else runs. This page runs one for
you: a control plane, a router and a web console on your laptop, in one
process, with two nodes talking through it. `sam-one` runs the same code as a
Kubernetes deployment, so what you learn here applies there too.

You need the `sam-one`, `sam-node` and `mcp-client` binaries. The
[install script](../quickstart/#1-install) provides all three.

## 1. Start the control plane

```bash
sam-one --data-dir ~/sam-one
```

`--data-dir` holds the database, the router's key and the generated tokens.
If you delete it, you get a new mesh. After a moment `sam-one` prints a
banner:

```text
══════════════════════════════════════════════════════════════════
SAM standalone mesh is ready!

API URL:      http://0.0.0.0:33775
Web Console:  http://0.0.0.0:33775/console
Router Peer:  12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY
Admin Token:  sam_adm_…
Join Token:   sam_tok_…

To enroll a node:
  sam-node join http://0.0.0.0:33775 --bootstrap-token-path /home/you/sam-one/join-token
══════════════════════════════════════════════════════════════════
```

`sam-one` picked a free port. Pass `--port 8080` for a fixed one. The **join
token** is a standing bootstrap token that lets nodes enroll without an
identity provider. The **admin token** authenticates the console and the
admin API. Both are also written to files in the data directory, and the rest
of this page reads them from there.

On first boot, `sam-one` seeds an open development policy and logs a warning
about it: any enrolled node may register any service and call any service.
This is a reasonable default for a laptop and a bad one for anything shared.
The last section explains how to replace it.

## 2. Publish a service from one node

Any HTTP backend can be a mesh service. Start a simple one and declare it in
a node configuration file:

```bash
mkdir -p /tmp/www && echo "hello from node a" > /tmp/www/hello.txt
python3 -m http.server 9000 --bind 127.0.0.1 --directory /tmp/www &

cat > ~/node-a.yaml <<'EOF'
version: "v1alpha1"
services:
  - type: mcp
    name: hello
    description: "a static file, served over the mesh"
    target_url: "http://127.0.0.1:9000"
EOF
```

Then run a node with it. Substitute the port from your banner:

```bash
URL=http://127.0.0.1:33775

sam-node run --control-plane $URL \
  --bootstrap-token-path ~/sam-one/join-token \
  --config ~/node-a.yaml \
  --data-dir ~/node-a --bind-addr= --allow-loopback --listen /ip4/127.0.0.1/tcp/0
```

Three of these flags are only needed because both nodes run on one machine.
`--bind-addr=` (an empty value) keeps the node's local API on its Unix socket,
so the two nodes do not compete for port 8080. `--allow-loopback` lets the
nodes advertise and dial `127.0.0.1`. `--listen .../tcp/0` picks a free
peer-to-peer port. On separate machines you would not pass any of them.

The node enrolls with the join token, connects to the router and prints its
peer ID:

```text
SAM Node Online.
PeerID: 12D3KooWSCnbUoZ8Jv3EKGv17LqEWtnTMfZ3XYJUg2WTm5Gz2hUK
```

Keep that value.

## 3. Call it from another node

In a second terminal, start a node with no services:

```bash
URL=http://127.0.0.1:33775

sam-node run --control-plane $URL \
  --bootstrap-token-path ~/sam-one/join-token \
  --data-dir ~/node-b --bind-addr= --allow-loopback --listen /ip4/127.0.0.1/tcp/0
```

Every node's local API includes a proxy at `/sam/<peer-id>/<type>/<name>/`
that forwards to a service on another node. Ask node B for node A's file:

```bash
PEER_A=12D3KooWSCnbUoZ8Jv3EKGv17LqEWtnTMfZ3XYJUg2WTm5Gz2hUK

curl --unix-socket ~/node-b/sam.sock "http://localhost/sam/$PEER_A/mcp/hello/hello.txt"
# hello from node a
```

The request went from B's socket to B, then over an authenticated connection
to A, through A's policy check, to the Python server, and back. If A and B
cannot reach each other directly, the connection is relayed through the
router inside `sam-one`. Allow a few seconds after A starts for B to learn
about it. Nodes announce their services in a discovery table that the router
hosts, and the announcement takes a moment to arrive.

An MCP backend works the same way. The difference is that the node
understands the protocol: the service appears in `discover_remote_services`
and its tools in `find_remote_tools`, as on the testnet.

## 4. Look at it in the console

Open `http://127.0.0.1:33775/console` and paste the admin token. The console
shows the enrolled nodes, the router, the services each node reports, the
bootstrap tokens with their remaining uses, and the mesh policy. You can edit
the policy in place.

The same operations are available from the command line. `sam-one` is also
an admin client for a running server. It reads the admin token from the data
directory, from `SAM_ADMIN_TOKEN`, or from `--admin-token-path`, never from a
flag value:

```bash
sam-one token list   --server $URL --data-dir ~/sam-one
sam-one token create --server $URL --data-dir ~/sam-one --description "node c" --max-usages 1
sam-one token revoke <token-id> --server $URL --data-dir ~/sam-one
sam-one admin ban <peer-id>      --server $URL --data-dir ~/sam-one
```

## Reaching it from other machines

Everything above used `http://127.0.0.1`. `sam-node` accepts a plain `http`
URL only when the control plane is on the same host. A node on another
machine needs an `https` URL, because the control plane is the node's trust
root and SAM refuses to fetch it over plaintext from a remote address.

On a laptop behind NAT, the quickest way to get an `https` URL is a temporary
tunnel:

```bash
sam-one --data-dir ~/sam-one --tunnel cloudflare
```

This publishes the port on a random `trycloudflare.com` hostname, with no
account needed, and prints that URL in the banner. If `cloudflared` is not
installed, `sam-one` offers to download a pinned, checksum-verified release
into the data directory. `--tunnel-install` accepts the download without
asking. Nodes on any network then enroll with the same
`sam-node run --control-plane <url> --bootstrap-token-path <file>` command,
without the loopback flags.

If you have a real hostname and a reverse proxy in front, pass
`--external-url https://mesh.example.com` instead. The
[Cloud Run guide](../../guides/cloud-run/) shows a hosted variant.

With `--tunnel` or any other `https` URL, `sam-one` also prints a QR code
that enrolls a phone running the SAM Connect app. That app is in
[preview](../../preview/mobile/).

## Before you rely on it

- **Policy**: replace the open development policy. Write a
  [mesh policy](../../reference/policy/) file and start `sam-one` with
  `--policy-file` on a fresh data directory, or edit the policy in the
  console. Once the database has a policy, the database is the source of
  truth.
- **Enrollment**: run with `--no-join-token` so nodes can only enroll with
  tokens you mint (`sam-one token create`, single-use by default), or give
  the mesh an identity provider with `--issuer` and let people log in.
- **State**: keep the data directory on durable storage, or point
  `--db-driver postgres --db-dsn ...` at a database.
- **Tokens in logs**: read the admin token from `SAM_ADMIN_TOKEN` or
  `--admin-token-path` instead of the banner, and pass `--enroll-qr=false`
  in non-interactive environments.

The [sam-one reference](../../reference/sam-one/) lists every flag.
