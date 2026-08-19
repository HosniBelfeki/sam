#!/bin/bash
set -e

# Usage: ./scripts/build-rootfs.sh [GCS_URL]
GCS_URL=${1:-}

echo "Building Alpine Linux rootfs with Python, tun2proxy v0.8.3, and Chaos Agent..."
cat << 'EOF' > Dockerfile
FROM alpine:3.18
RUN apk add --no-cache python3 py3-pip openrc iproute2 socat curl

# Install tun2proxy
RUN curl -sSL -o /usr/local/bin/tun2proxy https://github.com/tun2proxy/tun2proxy/releases/download/v0.8.3/tun2proxy-linux-x86_64-musl \
    && chmod +x /usr/local/bin/tun2proxy

COPY cmd/chaos-agent /app/chaos-agent
RUN pip3 install --no-cache-dir -r /app/chaos-agent/requirements.txt --break-system-packages

COPY scripts/microvm-init.sh /sbin/init
RUN chmod +x /sbin/init
EOF

# Build the docker container
docker build -t chaos-rootfs -f Dockerfile .
CONTAINER_ID=$(docker create chaos-rootfs)
docker export $CONTAINER_ID > rootfs.tar
docker rm $CONTAINER_ID

# Create the ext4 image
dd if=/dev/zero of=rootfs.ext4 bs=1M count=500
mkfs.ext4 rootfs.ext4
mkdir -p /tmp/rootfs

# Use sudo to mount and untar, since we need root permissions to create files with correct ownership
sudo mount rootfs.ext4 /tmp/rootfs
sudo tar -xf rootfs.tar -C /tmp/rootfs
sudo umount /tmp/rootfs

rm rootfs.tar Dockerfile
echo "Rootfs built successfully at rootfs.ext4"

if [ -n "$GCS_URL" ]; then
    echo "Uploading rootfs.ext4 to $GCS_URL..."
    gcloud storage cp rootfs.ext4 "$GCS_URL/rootfs.ext4"
    echo "Upload complete!"
fi
