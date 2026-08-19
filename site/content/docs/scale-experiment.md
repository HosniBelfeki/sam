---
title: "Scale Experiment Guide"
description: "How to run the massive scale Firecracker Minion experiment on GCP"
weight: 100
---

## Overview

As part of hardening the SAM mesh, we designed a massive scale-out architecture to test thousands of parallel connections and adversarial agents dynamically discovering MCP tools over the mesh.

To do this cost-effectively, we utilize **Nested Virtualization** on Google Cloud Platform (GCP).

### Components

1. **GCP Minion VMs:** Large bare-metal-equivalent instances (e.g. `n2-standard-64`) running Debian with KVM enabled.
2. **Firecracker MicroVMs:** Extremely lightweight VMs running Alpine Linux + Python. We run dozens of these on each Minion VM.
3. **Chaos Agent (`cmd/chaos-agent`):** A custom Python LangChain Agent leveraging the MCP SDK. It connects to the host's `sam-box` gateway over a transparent `tun2proxy` + `socat` VSOCK bridge to natively fuzz the mesh.

---

## Step 1: Building the Rootfs

Instead of building the Alpine filesystem on every Minion, we build it once locally and upload it to a Google Cloud Storage bucket.

```bash
# Build the ext4 image containing Alpine, Python, tun2proxy, and chaos-agent
./scripts/build-rootfs.sh gs://my-sam-bucket/scale-test
```

**Output:**
```
Building Alpine Linux rootfs with Python, tun2proxy v0.8.3, and Chaos Agent...
[+] Building 0.2s (11/11) FINISHED
...
Rootfs built successfully at rootfs.ext4
Uploading rootfs.ext4 to gs://my-sam-bucket/scale-test...
Average throughput: 210.9MiB/s
Upload complete!
```

---

## Step 2: Provisioning the Minion VMs

Once the rootfs is in GCS, you can spawn any number of Minion VMs. The provisioning script builds your local Go binaries and injects them alongside a `startup-script.sh` that pulls everything down automatically upon boot.

```bash
# Provision 100 VMs using your local binaries
./scripts/provision-scale-vm.sh --prefix sam-minions --count 100 --local-binaries gs://my-sam-bucket/scale-test
```

**Output:**
```
Building local binaries for injection...
Uploading binaries to GCS: gs://my-sam-bucket/scale-test
...
Provisioning 100 GCP Scale Experiment VM(s) with prefix 'sam-minions' in us-central1-c (ipv6-project-379110)...
Successfully triggered VM creation. Applying Ops Agent policy...
```

*Note: If you encounter a `ZONE_RESOURCE_POOL_EXHAUSTED` error, simply change the `--zone` parameter to another zone in your region with more capacity.*

---

## Step 3: Launching the MicroVMs

The `startup-script.sh` on the Minions will automatically:
1. Verify KVM nested virtualization is enabled.
2. Download Firecracker v1.16.1.
3. Download the `rootfs.ext4` and your compiled `sam-node` / `sam-box` binaries.

To spawn the test matrix, simply SSH into the minion VM and run the launcher:

```bash
gcloud compute ssh sam-minions-1 --zone us-central1-c
sudo /opt/microvm/launch-microvms.sh 20
```

This will launch 20 parallel Firecracker instances. Network traffic from the guest's `chaos-agent` is transparently routed via `tun2proxy` out of the guest, over the Firecracker VSOCK, and into a dedicated `sam-box` process running on the host.

### Monitoring logs
To ensure cloud-init/startup finished properly on a newly created VM:
```bash
gcloud compute ssh sam-minions-1 --command="sudo tail -f /var/log/startup-script.log"
```
