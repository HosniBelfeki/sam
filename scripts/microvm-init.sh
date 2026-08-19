#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Setup localhost
ip link set dev lo up

# 1. Bridge local TCP to Host VSOCK (Firecracker CID 2 maps to host UDS)
# The host sam-box UDS will receive connections from this port
socat TCP-LISTEN:8080,fork,reuseaddr VSOCK-CONNECT:2:8080 &

# 2. Setup transparent network using tun2proxy
ip tuntap add dev tun0 mode tun
ip link set dev tun0 up
ip addr add 198.18.0.1/15 dev tun0
ip route add default via 198.18.0.1 dev tun0

# tun2proxy will catch all traffic on tun0 and forward to the socat TCP port (which goes to sam-box on host)
tun2proxy --tun tun0 --proxy http://127.0.0.1:8080 &

# Give network a second to settle
sleep 2

echo "Starting Chaos Agent Loop..."
cd /app/chaos-agent
python3 agent.py --mcp-url http://127.0.0.1:8080/mcp --inference-url http://127.0.0.1:8080/v1 > /var/log/agent.log 2>&1

echo "Agent completed. Powering off."
reboot -f
