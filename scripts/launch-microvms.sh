#!/bin/bash
set -e

COUNT=${1:-1}
WORKDIR="/opt/microvm"

if [ ! -d "$WORKDIR" ]; then
    echo "Error: $WORKDIR does not exist. Did cloud-init finish?"
    exit 1
fi

echo "=== Launching $COUNT MicroVMs ==="
for i in $(seq 1 $COUNT); do
    VM_ID="vm-$i"
    # Firecracker's vsock device multiplexes guest->host connections onto
    # "<uds_path>_<port>", so the boundary has to listen on that exact name for
    # the guest's connections to CID 2 port 1080 to arrive.
    VSOCK_UDS="/var/run/sam-$VM_ID.vsock"
    BOUNDARY_UDS="${VSOCK_UDS}_1080"
    NODE_UDS="/var/run/sam-node-$VM_ID.sock"
    API_SOCKET="/tmp/firecracker-$VM_ID.socket"

    # Clean up old sockets
    rm -f $VSOCK_UDS $BOUNDARY_UDS $NODE_UDS $API_SOCKET

    # The node owns the mesh identity; sam-box holds none and simply consumes
    # the node's API socket on the agent's behalf.
    mkdir -p /tmp/sam-node-$VM_ID
    sam-node run --data-dir "/tmp/sam-node-$VM_ID" \
                 --control-plane "https://bananas.sam-mesh.dev" \
                 --bind-addr "" --socket-path "$NODE_UDS" \
                 --log-level debug > /var/log/sam-node-$VM_ID.log 2>&1 &

    sam-box run --socket "$BOUNDARY_UDS" --sidecar-socket "$NODE_UDS" \
                --log-level debug > /var/log/sam-box-$VM_ID.log 2>&1 &
    
    # Start Firecracker
    firecracker --api-sock $API_SOCKET > /var/log/fc-$VM_ID.log 2>&1 &
    sleep 1
    
    # Setup VM Kernel
    curl -s -X PUT --unix-socket $API_SOCKET \
         http://localhost/boot-source \
         -H 'Content-Type: application/json' \
         -d "{
             \"kernel_image_path\": \"$WORKDIR/vmlinux.bin\",
             \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off\"
         }" > /dev/null
         
    # Setup VM Rootfs (using a copy so it's writable per VM)
    cp $WORKDIR/rootfs.ext4 $WORKDIR/rootfs-$VM_ID.ext4
    curl -s -X PUT --unix-socket $API_SOCKET \
         http://localhost/drives/rootfs \
         -H 'Content-Type: application/json' \
         -d "{
             \"drive_id\": \"rootfs\",
             \"path_on_host\": \"$WORKDIR/rootfs-$VM_ID.ext4\",
             \"is_root_device\": true,
             \"is_read_only\": false
         }" > /dev/null
         
    # Setup VM VSOCK
    # Guest CID 3 is typically the first available guest CID. Connections the
    # guest makes to CID 2 surface on the host as "<uds_path>_<port>".
    curl -s -X PUT --unix-socket $API_SOCKET \
         http://localhost/vsock \
         -H 'Content-Type: application/json' \
         -d "{
             \"vsock_id\": \"vsock0\",
             \"guest_cid\": 3,
             \"uds_path\": \"$VSOCK_UDS\"
         }" > /dev/null
         
    # Start VM
    curl -s -X PUT --unix-socket $API_SOCKET \
         http://localhost/actions \
         -H 'Content-Type: application/json' \
         -d '{ "action_type": "InstanceStart" }' > /dev/null
         
    echo "Started MicroVM $VM_ID via VSOCK -> sam-box gateway."
done

echo "=== All MicroVMs running! ==="
