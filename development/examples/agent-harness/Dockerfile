# A sandbox image holds the harness and the plumbing that gives it a route.
# No credentials, and no proxy configuration: the agent is unmodified and does
# not know it is confined.
FROM python:3.12-slim

# socat bridges the boundary's Unix socket to a port, iproute2 builds the tun,
# and tun2proxy makes that tun the way out. This is the same arrangement a
# microVM runs, which is the point: the agent above it is identical either way.
ARG TUN2PROXY_VERSION=v0.8.3
RUN apt-get update \
  && apt-get install -y --no-install-recommends socat iproute2 ca-certificates curl unzip \
  && curl -sSL -o /tmp/tun2proxy.zip \
     "https://github.com/tun2proxy/tun2proxy/releases/download/${TUN2PROXY_VERSION}/tun2proxy-x86_64-unknown-linux-musl.zip" \
  && unzip -j /tmp/tun2proxy.zip tun2proxy-bin -d /usr/local/bin \
  && mv /usr/local/bin/tun2proxy-bin /usr/local/bin/tun2proxy \
  && chmod +x /usr/local/bin/tun2proxy \
  && apt-get purge -y curl unzip && apt-get autoremove -y \
  && rm -rf /var/lib/apt/lists/* /tmp/tun2proxy.zip

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY agent.py entrypoint.sh ./
RUN chmod +x entrypoint.sh

# Building the tun needs NET_ADMIN, so this stays root. The isolation that
# matters is the sandbox around the container, not the uid inside it; a
# microVM makes the same trade with its own kernel.
ENTRYPOINT ["./entrypoint.sh"]
