#!/bin/sh
# PID 1 in an agent sandbox.
#
# Its whole job is to give the guest a default route that leads to the sandbox
# boundary, and then get out of the way. What it deliberately does not do is
# reach into the agent: no proxy variables in its environment, no resolver
# rewritten under it, nothing preloaded into its address space. An agent that
# has to cooperate with its own confinement is not confined, because the next
# library that ignores the convention, or the next subprocess that clears the
# environment, is outside it.
#
# So the agent below is unmodified. It makes ordinary requests to ordinary
# names, and every one of them leaves through tun0 because there is nowhere
# else for them to go.
set -eu

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

ip link set dev lo up

# The boundary is a Unix socket on the host. Firecracker's vsock multiplexes
# guest connections onto "<uds_path>_<port>", so a connection to CID 2 port
# 1080 arrives on the host at the path sam-box serves. socat is only plumbing:
# it decides nothing and sees nothing the boundary would not.
socat TCP-LISTEN:1080,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:1080 &

# One interface, one route, and it leads to the boundary. There is no NIC in
# this guest, so this is not the preferred way out; it is the only one.
#
# Every address here is link-local, because that is what these addresses are
# for: RFC 3927 describes a single link with no router, which is exactly what a
# tun to the boundary is. Nothing is routable and nothing can be mistaken for a
# real destination — a sandbox numbered out of 10.0.0.0/8 would eventually be
# deployed somewhere that already uses it.
ip tuntap add dev tun0 mode tun
ip link set dev tun0 up
ip addr add 169.254.1.1/30 dev tun0
ip route add default dev tun0

# The resolver address is a fiction that never receives a packet: virtual DNS
# answers inside tun2proxy and hands the name to the boundary instead. That is
# the point — mesh.sam.alt has no address of its own, and the boundary is what
# turns the name into a provider.
echo "nameserver 169.254.1.53" > /etc/resolv.conf

# socks5, because that is what sam-box speaks. The previous version of this
# script said http, which the boundary has never spoken, and then had the agent
# call the bridge directly, which went around the tun it had just built.
tun2proxy --tun tun0 --proxy socks5://127.0.0.1:1080 --dns virtual \
  --dns-addr 169.254.1.53 --virtual-dns-pool 169.254.64.0/18 &

# Wait for the route to exist, so the agent's first request fails for real
# reasons rather than for being early.
for _ in $(seq 1 100); do
  [ -d /sys/class/net/tun0 ] && break
  sleep 0.05
done

echo "Sandbox ready; starting agent." > /dev/console

# The agent is given a task and nothing else. It reaches the mesh by name,
# holds no credentials, and does not know it is in a virtual machine.
cd /app/agent
python3 agent.py "${AGENT_TASK:-Describe the tools you have and what each is for.}" \
  > /dev/console 2>&1 || echo "agent exited non-zero" > /dev/console

echo "Agent finished. Powering off." > /dev/console
reboot -f
