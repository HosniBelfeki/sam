---
title: "Device Enrollment (QR Code)"
linkTitle: "Device Enrollment"
weight: 7
---

# Enrolling Phones with a QR Code

A phone joins the mesh the same way a `sam-node` does: it presents a
bootstrap token to the control plane's `POST /enroll`, which spends the
token and issues the device a Biscuit bound to its own key. The only thing
a QR code adds is a way to hand the phone the two facts it needs without
typing them:

```
sam://enroll?server=<control-plane-url>&token=<bootstrap-token>
```

The token is an ordinary bootstrap token, so the code grants exactly what
the token grants — one enrollment, in the token's role, until it expires —
against any SAM control plane, `sam-one` or a full `sam-control-plane`.

## 1. Run `sam-one` on a public https URL

Phones only trust an **https** control plane (plaintext `http://` is
accepted for loopback only): whoever answers that URL becomes the device's
trust root, so `sam-one` refuses to draw a QR code for a plaintext address
and tells you how to get one. Three ways, from a laptop to production:

**Laptop behind NAT — a tunnel.** `sam-one` can publish its single port on
a temporary public hostname through a tunnel provider. The first one is
[Cloudflare Quick Tunnels](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/),
which need no account:

```bash
sam-one --data-dir ~/sam-one --tunnel cloudflare
```

If `cloudflared` is not on your `PATH`, `sam-one` offers to download the
pinned release from GitHub into `~/sam-one/bin`. The download is verified
against a SHA-256 digest compiled into `sam-one`, re-verified before every
launch, and only happens after you accept Cloudflare's license at the
prompt (or pass `--tunnel-install` to accept up front, for scripts). Pass
`--cloudflared-path` to use a copy you installed yourself.

Quick tunnels are for testing and demos: no uptime guarantee, a new
hostname on every start, and no Server-Sent Events. Everything else works —
enrollment, the web console, and the `wss` mesh transport nodes use to
reach the embedded router.

**Cloud Run** terminates TLS for you; follow the
[Cloud Run deployment guide](../cloud-run-deployment/) and set
`SAM_EXTERNAL_URL` to the service URL.

**Your own VM and domain — a reverse proxy.** Put an ACME-capable proxy in
front of the single port and tell `sam-one` its public name:

```bash
# Caddyfile: Let's Encrypt certificate, automatic renewal, WebSockets included.
mesh.example.com {
    reverse_proxy 127.0.0.1:8080
}

sam-one --data-dir /var/lib/sam-one --port 8080 --external-url https://mesh.example.com
```

## 2. Read the code

With a public https URL, `sam-one` prints a device enrollment block under
its startup banner whenever stdout is a terminal (`--enroll-qr=false` to
suppress it, `--enroll-qr` to force it):

```
Scan with the SAM app to enroll a device into brave-otter-quick-1234.trycloudflare.com
(single use, valid for 1h0m0s):

█████████████████████████████████████
████ ▄▄▄▄▄ █▀ █▀▀▄▀▄▀▀ █ ▄▄▄▄▄ ████
...

sam://enroll?server=https%3A%2F%2Fbrave-otter-quick-1234.trycloudflare.com&token=sam_dev_...
```

Each code carries a fresh node token, minted for that boot and valid for
one hour. By default it is **single use**: once a device has spent it,
`POST /enroll` refuses it for anyone else. It is never persisted, so a
stale screenshot is worthless.

For more devices, or on a non-interactive deployment, mint a code on demand
with the admin CLI:

```bash
# Admin credential: env, or --admin-token-path <file>; never a flag value.
export SAM_ADMIN_TOKEN=...      # from the banner, or <data-dir>/admin-token
sam-one token qr --server https://brave-otter-quick-1234.trycloudflare.com
```

`--server` is both where the CLI talks to and the URL embedded in the code;
pass `--enroll-url https://...` when devices reach the mesh on a different
address than the admin does. `sam-one token list` shows every minted token
with its usage count and status, and the standard `--role`/`--ttl-hours`
tokens from `sam-one token create` work in the app's manual entry too.

### One code for a whole room

A demo with an audience does not want one code per phone. Give the code a
usage budget instead, and every device that scans it joins until the
budget, the TTL or you say otherwise:

```bash
# Startup code good for 200 devices (projector-friendly).
sam-one --data-dir ~/sam-one --tunnel cloudflare --enroll-qr-max-usages 200

# Or mint one for the afternoon on a running mesh.
sam-one token qr --server "$URL" --max-usages 200 --ttl-hours 4
```

The caption under the code says how many devices it admits and prints the
token id. Anyone who photographs a shared code can enroll until it is
exhausted, so end it when the session ends:

```bash
sam-one token list --server "$URL"          # STATUS: active / exhausted / expired / revoked
sam-one token revoke 9c75ab4a3122 --server "$URL"
```

Revoking a token stops further enrollments only; the devices it already
admitted keep their identity. Remove one of those with
`sam-one admin ban <peer-id>`, or, for a throwaway demo mesh, simply stop
`sam-one` (a fresh data dir is a fresh mesh).

On a full `sam-control-plane`, mint the token with
`POST /admin/bootstrap-tokens` (`max_usages: 1`) and build the same
`sam://enroll?...` string; the app does not care which binary is behind the
URL.

## 3. Enroll the phone

In **SAM Connect** (the Android app):

1. Tap **Scan enrollment code** and point the camera at the terminal. The
   app only accepts `sam://enroll` codes; any other QR is ignored with a
   hint.
2. Confirm the control plane hostname in the dialog and tap **Join**. Labels
   set on the **Config** tab are attested into the device's Biscuit at this
   point.
3. Start the node from the dashboard.

Scanning with the phone's stock camera app works as well: the `sam://`
link opens SAM Connect with the same confirmation dialog. **Enter details
manually** covers everything else: paste the whole `sam://enroll` link or a
bare token (with the control plane URL), or use the browser and device
OIDC logins where the control plane is configured with an identity
provider.

A device that joined with a token has no login session to refresh, so its
labels cannot be re-attested in place: **Re-enroll** is disabled for it,
and changing labels means unenrolling and scanning a new code.

## What the token does and does not do

* One usage, one device. `POST /enroll` consumes a usage atomically before
  anything is minted, so devices racing on the last usage of a code cannot
  both win. The loser sees "Bootstrap token expired, revoked or exhausted".
* The device keeps its identity, not the token. Its Biscuit is bound to the
  key it generated locally and is refreshed through `/refresh`; revoking it
  later is `sam-one admin ban <peer-id>`, exactly as for any node.
* The token is scoped to the `sam:role:node` role. It cannot mint other
  tokens, approve enrollments or reach the admin API; the admin token never
  leaves the operator's terminal.
* The code binds server and token together but the control plane does not
  pin a token to a hostname: a token is valid at whatever URL the same
  control plane answers on. Treat the printed line like the join token —
  it is a credential until it is spent or expires.

## From demo to fleet

`sam-one` is the same control plane, router and store as the split
deployment in one process, so a device enrolled through a QR code gets the
production credential lifecycle: a Biscuit bound to its own key, renewed
through `POST /refresh` with a signed challenge well before its 24 h
expiry, verified against rotating signing keys, revocable with
`sam-one admin ban`. The mobile app runs the same renewal loop as
`sam-node`. What separates a demo from a fleet is configuration:

* **Drop the standing join token.** The auto-generated join token is a
  ten-year, unlimited-use secret meant for `sam-node join` on a laptop.
  Run with `--no-join-token`; devices then enroll only with tokens you
  mint (`token qr`, `token create`) or through OIDC.
* **Seed a real policy.** Without `--policy-file` the first boot installs
  an open development policy (any enrolled node may declare any label and
  register any service) and says so in the log. Use
  `--control-plane-manual-enrollment` if each device should be approved in
  the console before it gets a credential.
* **Pass secrets, don't read them off the screen.** Secrets are only ever
  read from a file (`--admin-token-path`, `--token-path`) or the environment
  (`SAM_ADMIN_TOKEN`, `SAM_TOKEN`); there is deliberately no flag that takes
  the value, so it never lands in `ps` output or shell history. When you
  supply them, the banner names the source instead of echoing the value into
  your logs. `--enroll-qr=false` keeps startup codes out of non-interactive
  logs.
* **Give the mesh a stable https name.** A quick tunnel changes hostname on
  every start, and enrolled devices remember the URL they enrolled at —
  fine for an afternoon, fatal for a fleet. Use Cloud Run, a named tunnel
  on your own domain, or a reverse proxy with `--external-url`.
* **Decide what happens to devices that go dark.** The signing key rotates
  every 24 h and stays valid for verification for a 1 h grace period. A
  device that renews inside that window is fine indefinitely; one that
  comes back later holds a Biscuit no current key can verify, and
  `/refresh` refuses it — the node exits rather than run unverified. Two
  knobs, and they are a trade-off:
  * Mint its token with `--autonomous-recovery`. The device may then
    recover on proof of possession of its key alone, whenever it returns.
    The cost is that a lost device keeps renewing until you ban it.
  * Or widen the window for everyone with
    `--control-plane-key-grace-period` (e.g. `168h` for a weekly check-in
    cadence). The cost is that a stolen credential also lives that long.
  Toggle recovery per device later from the console
  (`POST /admin/nodes/{peer-id}/autonomous-recovery`).
* **Keep the data directory.** `router.key` is the mesh's identity;
  `sam.db` holds enrollments, tokens and policy. Back it up, or point
  `--db-driver postgres` at a managed database. `sam-one` is a singleton by
  design; when you need more than one control plane, move to the split
  components with the Helm chart.

Want OIDC instead of tokens? Give `sam-one` an issuer (`--issuer`,
`--allowed-audiences`, `--oidc-client-id`) and the app's **Login & Enroll**
and **Device Login** flows work unchanged, as does signing in to the
console as an OIDC admin.
