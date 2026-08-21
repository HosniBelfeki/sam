#!/bin/bash
# Launch N agent sandboxes on one host.
#
# The shape here is the point, and it is not what this script used to do. It
# ran one sam-node per microVM, which made every agent a mesh member: its own
# enrolment, its own libp2p host, its own place in the DHT. That does not reach
# a thousand agents on a host, and it measures the wrong thing besides. An
# agent is a principal, not a peer.
#
# So: one sam-node for the host, which is the mesh member, and one sam-box per
# agent, which holds no mesh identity at all and names its agent on every
# request. Adding an agent costs a boundary, not an enrolment.
set -euo pipefail

COUNT=${1:-1}
WORKDIR="${WORKDIR:-/opt/microvm}"
CONTROL_PLANE="${CONTROL_PLANE:-https://bananas.sam-mesh.dev}"
AGENT_DOMAIN="${AGENT_DOMAIN:-scale.sam-mesh.dev}"

# Firecracker defaults to 128 MiB, which silently decides how many agents fit
# on a host. Saying it out loud makes density a parameter of the experiment
# rather than a property of the tool.
#
# 160 is the smallest size measured to run the example Python harness at full
# speed: 144 works but boots a second slower for being cramped, and 136 never
# reaches the boundary at all. A heavier agent needs more, so measure your own
# rootfs with tests/scale/measure-guest.sh rather than trusting a number
# written for this one.
#
# Worth knowing when budgeting: mem_size_mib is a ceiling, not an allocation.
# The host only pays for pages the guest touches, so raising it is cheap and
# the real cost per agent is roughly the guest's working set plus about 18 MiB
# for its sam-box.
VM_MEM_MIB="${VM_MEM_MIB:-160}"
VM_VCPUS="${VM_VCPUS:-1}"

if [ ! -d "$WORKDIR" ]; then
    echo "Error: $WORKDIR does not exist. Did cloud-init finish?" >&2
    exit 1
fi

# Everything the host writes is overridable, so this can be exercised on a
# workstation before it is trusted on a fleet. The defaults are the paths a
# provisioned VM has.
NODE_UDS="${NODE_UDS:-/var/run/sam-node.sock}"
NODE_DIR="${NODE_DIR:-/var/lib/sam-node}"
RUN_DIR="${RUN_DIR:-/var/run}"
LOG_DIR="${LOG_DIR:-/var/log}"
SAM_NODE="${SAM_NODE:-sam-node}"
SAM_BOX="${SAM_BOX:-sam-box}"

mkdir -p "${RUN_DIR}" "${LOG_DIR}"

echo "=== One node for the host ==="
# An existing socket is taken as an existing node. That makes this safe to run
# twice, and lets an operator start the node however their environment needs
# rather than only the way this script would.
if [ -S "${NODE_UDS}" ]; then
    echo "Using the node already serving ${NODE_UDS}"
else
    mkdir -p "${NODE_DIR}"
    "${SAM_NODE}" run \
        --data-dir "${NODE_DIR}" \
        --control-plane "$CONTROL_PLANE" \
        --bind-addr "" \
        --socket-path "${NODE_UDS}" \
        > "${LOG_DIR}/sam-node.log" 2>&1 &

    # The boundaries are useless before the node answers, and starting a
    # thousand of them against a socket that does not exist yet produces a
    # thousand identical failures instead of one clear one.
    for _ in $(seq 1 600); do
        [ -S "${NODE_UDS}" ] && break
        sleep 0.1
    done
    [ -S "${NODE_UDS}" ] || {
        echo "node never bound ${NODE_UDS}" >&2
        tail -20 "${LOG_DIR}/sam-node.log" >&2
        exit 1
    }
    echo "Node ready at ${NODE_UDS}"
fi

fc_put() {
    curl -sf -X PUT --unix-socket "$1" \
        "http://localhost$2" -H 'Content-Type: application/json' -d "$3" > /dev/null
}

echo "=== Launching $COUNT agent sandboxes ==="
for i in $(seq 1 "$COUNT"); do
    VM_ID="vm-$i"
    # Firecracker's vsock multiplexes guest connections onto
    # "<uds_path>_<port>", so the boundary must listen on that exact name for
    # the guest's connections to CID 2 port 1080 to arrive.
    VSOCK_UDS="${RUN_DIR}/sam-$VM_ID.vsock"
    BOUNDARY_UDS="${VSOCK_UDS}_1080"
    API_SOCKET="/tmp/firecracker-$VM_ID.socket"
    BUNDLE="${RUN_DIR}/sam-$VM_ID.bundle.yaml"

    rm -f "$VSOCK_UDS" "$BOUNDARY_UDS" "$API_SOCKET"

    # Each sandbox is a different agent, because a thousand sandboxes sharing
    # one identity would tell the mesh it is serving one agent very hard. The
    # identifier is dot-separated so one rule can match the whole population:
    # *.scale.sam-mesh.dev admits these and nothing else.
    #
    # An empty egress allowance is the interesting default: these agents reach
    # the mesh and nothing else, so anything they get to is something policy
    # granted.
    cat > "$BUNDLE" <<EOF
version: v1
agent:
  id: agent-${i}.${AGENT_DOMAIN}
egress:
  allow: []
EOF

    # There is no credential issuer in this harness, and the flag says so
    # rather than a default quietly meaning it.
    "${SAM_BOX}" run \
        --socket "$BOUNDARY_UDS" \
        --sidecar-socket "${NODE_UDS}" \
        --bundle "$BUNDLE" \
        --insecure-unverified-bundle \
        > "${LOG_DIR}/sam-box-$VM_ID.log" 2>&1 &

    firecracker --api-sock "$API_SOCKET" > "${LOG_DIR}/fc-$VM_ID.log" 2>&1 &

    for _ in $(seq 1 100); do
        [ -S "$API_SOCKET" ] && break
        sleep 0.05
    done

    fc_put "$API_SOCKET" /boot-source "{
        \"kernel_image_path\": \"$WORKDIR/vmlinux.bin\",
        \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off\"
    }"

    fc_put "$API_SOCKET" /machine-config "{
        \"vcpu_count\": $VM_VCPUS,
        \"mem_size_mib\": $VM_MEM_MIB
    }"

    cp "$WORKDIR/rootfs.ext4" "$WORKDIR/rootfs-$VM_ID.ext4"
    fc_put "$API_SOCKET" /drives/rootfs "{
        \"drive_id\": \"rootfs\",
        \"path_on_host\": \"$WORKDIR/rootfs-$VM_ID.ext4\",
        \"is_root_device\": true,
        \"is_read_only\": false
    }"

    # Guest CID 3 is the first available guest CID. Connections the guest makes
    # to CID 2 surface on the host as "<uds_path>_<port>".
    fc_put "$API_SOCKET" /vsock "{
        \"vsock_id\": \"vsock0\",
        \"guest_cid\": 3,
        \"uds_path\": \"$VSOCK_UDS\"
    }"

    fc_put "$API_SOCKET" /actions '{ "action_type": "InstanceStart" }'

    echo "Started $VM_ID as agent-${i}.${AGENT_DOMAIN} (${VM_VCPUS} vCPU, ${VM_MEM_MIB} MiB)"
done

echo "=== $COUNT sandboxes running against one node ==="
echo "How many agents the node thinks it is serving:"
echo "  grep sam_node_agents_seen <(curl -s --unix-socket $NODE_UDS http://localhost/metrics)"
