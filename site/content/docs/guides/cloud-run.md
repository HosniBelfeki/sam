---
title: "Cloud Run"
linkTitle: "Cloud Run"
weight: 5
aliases:
  - /docs/user/cloud-run-deployment/
---

`sam-one` serves its HTTP API, the console and the router's WebSocket
transport on one port. That is the shape Cloud Run expects. This guide
deploys it there, joins nodes from anywhere, and lists what changes when
`sam-one` runs on Cloud Run. The same steps apply to any platform that
forwards HTTP and WebSockets to a container.

## 1. Build and push

```bash
PROJECT=my-project
REGION=us-central1
IMG=${REGION}-docker.pkg.dev/${PROJECT}/sam-mesh/sam-one:latest

docker build -f Dockerfile.sam-one -t "$IMG" .
gcloud auth configure-docker ${REGION}-docker.pkg.dev
docker push "$IMG"
```

## 2. Deploy

Cloud Run has no persistent disk, so the join and admin tokens are set
through environment variables. Without this, a new instance would generate
new tokens and nodes that enrolled earlier could not renew.

```bash
JOIN_TOKEN="sam_tok_$(openssl rand -hex 16)"
ADMIN_TOKEN="sam_adm_$(openssl rand -hex 16)"

gcloud run deploy sam-one \
  --project "$PROJECT" --region "$REGION" \
  --image "$IMG" \
  --allow-unauthenticated \
  --min-instances 1 --max-instances 1 \
  --port 8080 \
  --no-cpu-throttling \
  --timeout 3600 \
  --set-env-vars "SAM_TOKEN=${JOIN_TOKEN},SAM_ADMIN_TOKEN=${ADMIN_TOKEN}"
```

Each flag is needed:

- `--min-instances 1 --max-instances 1`: the router's DHT and relay state
  live in the one process. Two instances would be two separate meshes behind
  one URL.
- `--no-cpu-throttling`: the router runs background loops (lease renewal,
  key sync, DHT maintenance) between requests.
- `--timeout 3600`: Cloud Run limits the lifetime of a streaming request,
  and each node's WebSocket connection is one. Nodes reconnect when the limit
  closes a connection, but a low value causes unnecessary reconnects.
- `--allow-unauthenticated`: the mesh authenticates its own callers. Cloud
  Run's IAM check would block nodes before they could present a credential.

The router needs to know its public URL so that it can advertise an address
that nodes can dial. The URL only exists after the first deploy:

```bash
URL=$(gcloud run services describe sam-one --project "$PROJECT" --region "$REGION" \
  --format='value(status.url)')

gcloud run services update sam-one --project "$PROJECT" --region "$REGION" \
  --update-env-vars "SAM_EXTERNAL_URL=${URL}"
```

## 3. Verify

```bash
curl -s "$URL/readyz"              # 200
curl -s "$URL/info" | head -c 200  # contains /dns4/<host>/tcp/443/wss/p2p/<peer-id>
```

The console is at `$URL/console`. Use `/readyz` for probes. `/healthz` is
reserved by Cloud Run's frontend on `run.app` domains and never reaches the
container.

## 4. Join nodes

```bash
echo -n "$JOIN_TOKEN" > join-token
sam-node run --control-plane "$URL" --bootstrap-token-path join-token
```

The node enrolls over HTTPS, reads the router's `wss` address from `/info`,
and connects through the same port. Nodes behind NAT reach each other
through relay circuits on the Cloud Run instance, so neither node needs an
inbound port. Pass `--announce-private=false` to the nodes so that they do
not advertise LAN addresses that no remote peer can use.

Publishing and calling a service works as in
[your own mesh](../../getting-started/your-own-mesh/): declare the service in
`sam-node.yaml` on one node, and call it through
`/sam/<peer-id>/<type>/<name>/` on another.

## 5. Administer

`sam-one` is also the admin client. With `SAM_ADMIN_TOKEN` exported:

```bash
sam-one token create --server "$URL" --role sam:role:node --max-usages 1
sam-one token qr     --server "$URL"
sam-one token list   --server "$URL"
sam-one token revoke <token-id> --server "$URL"
sam-one admin ban <peer-id>     --server "$URL"
```

## What Cloud Run changes

- **State is ephemeral.** The SQLite database is in the container's memory.
  When an instance restarts, it forgets enrolled nodes and minted tokens,
  and the router gets a new peer ID. Nodes read `/info` again and re-enroll
  with the pinned join token. For state that survives restarts, point
  `--db-driver postgres --db-dsn <dsn>` at a managed database, or run
  `sam-one` on a VM with a disk.
- **Rollouts overlap.** During a deploy, `/info` may advertise the new
  instance while some WebSocket upgrades still reach the old one. Nodes
  detect the peer ID mismatch and retry. Joins succeed once the old revision
  has drained.
- **One source IP for everyone.** All traffic arrives from a small number of
  frontend proxies. `sam-one` raises libp2p's per-source-IP connection limit
  for this automatically. A separate `sam-router` behind a proxy needs
  `--conns-per-source-ip` raised by hand.
- **Policy.** First boot seeds the open development policy. Build a
  `--policy-file` into the image or post a policy after the deploy, and run
  with `--no-join-token` once you mint per-device tokens. The
  [sam-one reference](../../reference/sam-one/) lists the flags.
