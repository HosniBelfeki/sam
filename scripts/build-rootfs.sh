#!/bin/bash
set -e

# Usage: ./scripts/build-rootfs.sh [GCS_URL]
GCS_URL=${1:-}

echo "Building Alpine Linux rootfs with Python, tun2proxy v0.8.3, and Chaos Agent..."
cat << 'EOF' > Dockerfile
FROM alpine:3.18
RUN apk add --no-cache python3 py3-pip openrc iproute2 socat curl

# Install tun2proxy
RUN apk add --no-cache unzip \
    && curl -sSL -o /tmp/tun.zip https://github.com/tun2proxy/tun2proxy/releases/download/v0.8.3/tun2proxy-x86_64-unknown-linux-musl.zip \
    && unzip /tmp/tun.zip tun2proxy-bin -d /tmp/ \
    && mv /tmp/tun2proxy-bin /usr/local/bin/tun2proxy \
    && chmod +x /usr/local/bin/tun2proxy \
    && rm /tmp/tun.zip

COPY cmd/chaos-agent /app/chaos-agent
RUN pip3 install --no-cache-dir -r /app/chaos-agent/requirements.txt --break-system-packages

RUN rm -f /sbin/init
COPY --chmod=755 scripts/microvm-init.sh /sbin/init
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

# Use a privileged docker container to mount and populate the ext4 image without requiring sudo on the host
docker run --rm --privileged -v $(pwd):/work alpine sh -c '
    mkdir -p /mnt/rootfs
    mount /work/rootfs.ext4 /mnt/rootfs
    tar -xf /work/rootfs.tar -C /mnt/rootfs
    sync
    umount /mnt/rootfs || true
'

rm rootfs.tar Dockerfile
echo "Rootfs built successfully at rootfs.ext4"

if [ -n "$GCS_URL" ]; then
    echo "Uploading rootfs.ext4 to $GCS_URL..."
    gcloud storage cp rootfs.ext4 "$GCS_URL/rootfs.ext4"
    echo "Upload complete!"
fi
