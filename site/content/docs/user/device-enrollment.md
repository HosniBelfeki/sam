---
title: "A Mesh in 30 Seconds"
linkTitle: "A Mesh in 30 Seconds"
weight: 7
---

# A Mesh in 30 Seconds

You have a laptop and a phone. Neither has a public IP address, they are
not on the same network, and you want a program on the laptop to read a
sensor on the phone, securely, without first setting up a server, a
domain, a certificate or an identity provider.

That is what `sam-one` is for: the whole SAM control plane, router and
database in one binary on one port, and it prints a QR code the phone
scans to join. Everything below was run exactly as written and timed from
the moment `sam-one` started, on a fresh data directory:

```
 10.3s  sam-one: banner + QR printed
 13.7s  phone: enrolled
 18.8s  phone: node running, phone-sensors advertised
 19.6s  laptop: enrolled
 24.9s  laptop -> phone: {"battery_level": 64, "charging": true}
```

The taps on the phone were scripted; with a human reading the dialog it
lands around half a minute. The output is real, only the tokens are
shortened.

You need the `sam-one` and `sam-node` binaries on the laptop (`make` from
a checkout, or a [release](https://github.com/google/sam/releases)) and
**SAM Connect** on the phone, the Android app from the same release.

## 1. Start the control plane

```bash
sam-one --data-dir ~/sam-one --tunnel cloudflare
```

`--data-dir` holds the database, the router's identity and the generated
tokens; delete it and you have a brand new mesh. `--tunnel cloudflare`
publishes the single port on a temporary public `https` hostname through
a [Cloudflare quick tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/),
which needs no account. If `cloudflared` is not installed, `sam-one`
offers to download the pinned release (checksum-verified, into
`~/sam-one/bin`) once you accept Cloudflare's license at the prompt;
`--tunnel-install` accepts it up front.

A few seconds later (`sam-one` waits until the new hostname is actually
published in DNS before it prints anything, so what you see is live):

```
══════════════════════════════════════════════════════════════════
SAM standalone mesh is ready!

API URL:      https://fairy-museum-built-wing.trycloudflare.com
Tunnel:       https://fairy-museum-built-wing.trycloudflare.com -> http://0.0.0.0:33775
Web Console:  https://fairy-museum-built-wing.trycloudflare.com/console
Router Peer:  12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY
Admin Token:  sam_adm_…
Join Token:   sam_tok_…

To enroll a node:
  sam-node join https://fairy-museum-built-wing.trycloudflare.com --bootstrap-token-path /home/you/sam-one/join-token
══════════════════════════════════════════════════════════════════

Scan with the SAM app to enroll a device into fairy-museum-built-wing.trycloudflare.com
(single use, valid for 1h0m0s):

█████████████████████████████████████████████
████ ▄▄▄▄▄ ██▄ █ █ ▀ █▀▄▄▄▀  ▄▄▄ ▄▀▀ ▄▄▄▄▄ ████
████ █   █ █   ████ ▀▀▄▄█▀▀▀▄▀█▀▄▄ ▀ █   █ ████
████ █▄▄▄█ █  ▄███▀▀▀▀▀▄ ▄ ▄▄▄ █▄ ▄▀ █▄▄▄█ ████
                    …
sam://enroll?server=https%3A%2F%2Ffairy-museum-built-wing.trycloudflare.com&token=sam_dev_…
Token ID: 0b7acf609466 (revoke early with: sam-one token revoke 0b7acf609466)
```

That is a running mesh. The web console is live at the URL shown, the
join token lets `sam-node` processes in, and the QR code carries a fresh
**single-use** token for one phone.

## 2. Enroll the phone

Open SAM Connect, tap **Scan enrollment code** and point it at the
terminal. The app shows where the code leads before it spends the token;
tap **Join**.

| Confirm the mesh | Running |
|:---:|:---:|
| ![Join this mesh?](/images/sam-connect-join.png) | ![Node is Running](/images/sam-connect-running.png) |

On the **Services** tab switch **Battery Status** on, go back to the
dashboard and tap **Start**. The phone is now a node: it generated its own
key, was issued a credential bound to that key, connected to the router
through the tunnel and advertised a `phone-sensors` service to the mesh.
Note the **Node ID** on the dashboard; you will use it in a moment.

Scanning with the phone's stock camera app works too: the `sam://` link
opens SAM Connect with the same dialog. Without a camera, **Enter details
manually** takes the printed `sam://enroll…` line pasted as text.

## 3. Join the laptop and call the phone

```bash
sam-node run --control-plane https://fairy-museum-built-wing.trycloudflare.com \
  --bootstrap-token-path ~/sam-one/join-token \
  --data-dir ~/laptop-node --bind-addr=
```

The node enrolls with the join token the banner pointed at. An empty
`--bind-addr=` keeps its local API on a Unix socket only,
`~/laptop-node/sam.sock`, so nothing on the laptop needs a token to use
it. Now ask the phone for its battery, through the mesh:

```bash
PHONE=12D3KooWSCnbUoZ8Jv3EKGv17LqEWtnTMfZ3XYJUg2WTm5Gz2hUK   # Node ID from the phone's dashboard

curl -s --unix-socket ~/laptop-node/sam.sock \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_battery_status","arguments":{}}}' \
  http://localhost/sam/$PHONE/mcp/phone-sensors/
```

```json
{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"battery_level\": 46, \"charging\": true}"}]}}
```

That request left the laptop's socket, crossed the tunnel to the router
inside `sam-one`, was relayed to the phone over a connection the phone had
opened outbound, and came back the same way. Both devices are behind NAT;
neither accepted an inbound connection. Every hop was authenticated with
the credentials minted at enrollment, and the phone's node checked the
mesh policy before letting the call through.

Any MCP client can do the same: the path is
`/sam/<node-id>/mcp/<service>/` over that socket, and `tools/list` on it
shows what the phone offers (`get_battery_status`, and `get_location` once
you enable Location).

## 4. Add more devices

A second phone is another QR code. `sam-one token qr` mints one on a
running mesh, and `--max-usages 50` makes a single code, projected on a
screen, admit a whole room:

```bash
sam-one token qr --server https://fairy-museum-built-wing.trycloudflare.com --data-dir ~/sam-one
```

A second laptop gets a scoped, single-use token instead of the standing
join token:

```bash
# On the sam-one host: mint the token straight into a file.
sam-one token create --server https://fairy-museum-built-wing.trycloudflare.com \
  --data-dir ~/sam-one --description "second laptop" \
  | awk '/^Token:/{print $2}' > enroll-token

# On the second laptop, with that file copied over:
sam-node run --control-plane https://fairy-museum-built-wing.trycloudflare.com \
  --bootstrap-token-path enroll-token --data-dir ~/node --bind-addr=
```

The same `curl` from that laptop returns the same battery reading.
`sam-one token list` shows every token with its usage and status, and
`sam-one token revoke <id>` ends one early. A device that is already
enrolled stays enrolled until you say otherwise:
`sam-one admin ban <node-id>` removes it.

(`--data-dir` tells the CLI where to read the admin token; on another
machine, export `SAM_ADMIN_TOKEN` or pass `--admin-token-path` instead. It
is never a flag value.)

## Why this is not a toy

* **The phone's identity is a key it generated.** The token in the QR code
  is spent once at `POST /enroll` and worthless afterwards; what the phone
  keeps is a short-lived credential bound to its own key, renewed
  automatically and revocable by node ID. The app runs the same renewal
  loop as `sam-node`.
* **The URL must be `https`.** The control plane is each device's trust
  root, so SAM Connect refuses plaintext to anything but `localhost`, and
  `sam-one` will not print a QR code for a plaintext address. The tunnel
  is the zero-setup way to get `https` on a laptop; Cloud Run and a
  reverse proxy with `--external-url` are the others (see the [Cloud Run
  guide](../cloud-run-deployment/)).
* **Nothing is routed by IP.** Services are found by name and called by
  node ID; policy is evaluated on the service name, deny by default.
* **`sam-one` is the real thing.** It runs the same control plane, router
  and store as a Kubernetes deployment, in one process. What works here
  works there.

## Before you take it beyond a demo

A quick tunnel changes hostname every start and comes with no uptime
promise, and the first boot seeds an *open* development policy (the log
says so). For anything with more than a handful of devices:

* Give the mesh a stable `https` name and keep `~/sam-one`, or point
  `--db-driver postgres` at a database.
* Seed a real policy with `--policy-file`, and run with `--no-join-token`
  so devices enroll only with tokens you mint.
* Read the admin token from `SAM_ADMIN_TOKEN` or `--admin-token-path`
  rather than the banner; `--enroll-qr=false` keeps codes out of
  non-interactive logs.
* Decide what happens to devices that go dark for more than a day: widen
  `--control-plane-key-grace-period`, or mint their tokens with
  `--autonomous-recovery`. The trade-off is how long a lost device can
  keep renewing.
* Want OIDC instead of tokens? Give `sam-one` an issuer (`--issuer`,
  `--allowed-audiences`, `--oidc-client-id`) and the app's **Login &
  Enroll** and **Device Login** flows work unchanged.

Those are flags, not a different product.
