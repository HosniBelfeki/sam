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
    UDS_PATH="/var/run/sam-box-$VM_ID.sock"
    API_SOCKET="/tmp/firecracker-$VM_ID.socket"
    
    # Clean up old sockets
    rm -f $UDS_PATH $API_SOCKET
    
    # Start sam-box on the host, listening on a dedicated UDS for this VM
    # The sam-box acts as the gateway to the mesh.
    mkdir -p /tmp/sam-box-$VM_ID
    SAM_API_TOKEN="secret-token" sam-box run -u "$UDS_PATH" --data-dir "/tmp/sam-box-$VM_ID" --hub "https://bananas.sam-mesh.dev" --log-level debug > /var/log/sam-box-$VM_ID.log 2>&1 &
    
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
    # Guest CID 3 is typically the first available guest CID.
    # The host UDS will receive connections from the guest's socat.
    curl -s -X PUT --unix-socket $API_SOCKET \
         http://localhost/vsock \
         -H 'Content-Type: application/json' \
         -d "{
             \"vsock_id\": \"vsock0\",
             \"guest_cid\": 3,
             \"uds_path\": \"$UDS_PATH\"
         }" > /dev/null
         
    # Start VM
    curl -s -X PUT --unix-socket $API_SOCKET \
         http://localhost/actions \
         -H 'Content-Type: application/json' \
         -d '{ "action_type": "InstanceStart" }' > /dev/null
         
    echo "Started MicroVM $VM_ID via VSOCK -> sam-box gateway."
done

echo "=== All MicroVMs running! ==="
