#!/bin/sh
# /sbin/init in an agent microVM.
#
# All this does is name the boundary and the agent, because the kernel starts
# init with no arguments. Everything that used to be here -- the tun, the
# routes, the resolver, the proxy plumbing -- is nano-init's job now, in one
# tested binary shared with the container sandbox rather than sixty lines of
# shell that drifted out of step with the design twice.
#
# The boundary is a Unix socket on the host. Firecracker's vsock multiplexes
# guest connections onto "<uds_path>_<port>", so a guest connection to CID 2
# port 1080 arrives on the host at the path sam-box serves.
set -eu

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

exec /usr/local/bin/nano-init run vsock://2:1080 \
  python3 /app/agent/agent.py "${AGENT_TASK:-Describe the tools you have and what each is for.}" \
  > /dev/console 2>&1
