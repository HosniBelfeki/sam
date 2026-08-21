#!/bin/sh
# The sandbox's network, such as it is.
#
# This does the same job as the microVM's init: give the sandbox one route,
# leading to the boundary, and then leave the agent alone. In particular it
# sets no proxy variables. An agent that has to be told where its proxy is has
# to cooperate with its own confinement, and the next library that ignores the
# convention, or the next subprocess that clears the environment, is outside
# it. Here the agent is unmodified and its traffic leaves through tun0 because
# there is nowhere else for it to go.
set -eu

BOUNDARY_SOCKET="${BOUNDARY_SOCKET:-/run/agent.sock}"

if [ ! -S "${BOUNDARY_SOCKET}" ]; then
  echo "no boundary socket at ${BOUNDARY_SOCKET}: this sandbox has no way out" >&2
  exit 1
fi

# HTTP clients and tun2proxy both want a host:port; the boundary is a Unix
# socket. socat is plumbing only: it decides nothing, and every byte still
# arrives at the same socket for the boundary to rule on.
socat TCP-LISTEN:1080,bind=127.0.0.1,fork,reuseaddr "UNIX-CONNECT:${BOUNDARY_SOCKET}" &

# Everything the sandbox is given an address for is link-local, because that is
# what these addresses are: RFC 3927 describes a single link with no router,
# which is exactly what a tun to the boundary is. Nothing here is routable, and
# nothing here can be confused with a real destination — a sandbox numbered out
# of 10.0.0.0/8 would eventually be deployed somewhere that already uses it.
ip tuntap add dev tun0 mode tun
ip link set dev tun0 up
ip addr add 169.254.1.1/30 dev tun0
ip route add default dev tun0

# The resolver address is a fiction that never receives a packet: virtual DNS
# answers in tun2proxy and hands the name to the boundary. That is the point —
# mesh.sam.alt has no address, and the boundary is what turns a name into a
# provider.
echo "nameserver 169.254.1.53" > /etc/resolv.conf

tun2proxy --tun tun0 --proxy socks5://127.0.0.1:1080 --dns virtual \
  --dns-addr 169.254.1.53 --virtual-dns-pool 169.254.64.0/18 &

for _ in $(seq 1 100); do
  ip route show default | grep -q tun0 && break
  sleep 0.05
done

exec python agent.py "$@"
