# A sandbox image holds the harness and the init that gives it a route.
# No credentials, and no proxy configuration: the agent is unmodified and does
# not know it is confined.
FROM golang:1.26.5@sha256:705e964a93a2fd2e75c7d59bb7d781b57e30f12293ffde5175c69229e18fb678 AS init
WORKDIR /src
COPY cmd/nano-init/go.mod cmd/nano-init/go.sum ./
RUN go mod download
COPY cmd/nano-init/ ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /nano-init .

FROM python:3.12-slim@sha256:78387bc3881b8273120a12ebe6c1ab22b018ccc2c9adf565ae1ac9b536e184ea

# Nothing else is needed. nano-init carries its own TCP stack and speaks
# netlink directly, so this image has no tun2proxy, no socat and no iproute2 —
# which is the difference between a sandbox that is the agent and one that is
# the agent plus a toolbox.
COPY --from=init /nano-init /usr/local/bin/nano-init

WORKDIR /app
COPY development/examples/agent-harness/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY development/examples/agent-harness/agent.py .

# Building the tun needs NET_ADMIN, so this stays root. The isolation that
# matters is the sandbox around the container, not the uid inside it; a microVM
# makes the same trade with its own kernel.
ENTRYPOINT ["nano-init", "run", "/run/agent.sock", "python", "agent.py"]
