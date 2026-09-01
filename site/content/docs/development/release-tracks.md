---
title: "Release Tracks, Autoupdate, and Autoscaling"
linkTitle: "Release Tracks, Autoupdate, and Autoscaling"
---
Sovereign Agent Mesh (SAM) is deployed to public endpoints using automated environments, release tracks, and self-healing/scaling infrastructure.

---

## 1. Public Developer Testnets

> [!NOTE]
> **Developer Testnets Disclaimer:**
> Both public endpoints (`bananas.sam-mesh.dev` and `hub.sam-mesh.dev`) are **developer testnets created using community resources**. They are provided solely for testing, continuous integration, and rapid experimentation with the open-source codebase. They carry **no guarantees, no uptime commitments, and zero SLA**, and are not sovereign. For production workloads or strict data sovereignty, operators should deploy a dedicated mesh with customer-held keys using the [Helm chart](https://github.com/google/sam/tree/main/charts/sam-mesh) or [Kubernetes deployment guide](../user/kubernetes-deployment/). See [Digital & Data Sovereignty](../sovereignty/) for full architectural details.

The public developer deployment maintains two isolated testnet tracks:

### A. Bleeding-Edge Testnet Track (Bananas)
*   **Domain Name:** `bananas.sam-mesh.dev`
*   **Source Branch:** Tracks the `main` branch.
*   **Deployment Trigger:** Automatically deployed on every new push/commit to the `main` branch.
*   **Target Tag:** The deployment is tagged with the Git commit SHA (`github.sha`).
*   **Purpose:** Serves as the continuous integration and staging testbed for the latest unreleased features.

### B. Release-Tagged Testnet Track (Hub)
*   **Domain Name:** `hub.sam-mesh.dev`
*   **Source Branch:** Tracks semantic version tags matching `v*.*.*`.
*   **Deployment Trigger:** Automatically deployed whenever a new version tag is pushed to GitHub.
*   **Target Tag:** The deployment is tagged with the exact Git release tag (e.g. `v1.0.0`).
*   **Purpose:** Public testbed running tagged releases for developer interoperability testing.
---

## 2. Autoupdate Mechanism

Updates to both release tracks are fully automated via a robust **Continuous Deployment** pipeline:

1.  **GitHub Actions Trigger:** The workflow defined in [.github/workflows/deploy.yaml](../.github/workflows/deploy.yaml) is automatically triggered by repository events (pushing to main or pushing a version tag).
2.  **Determining the Track & Tag:**
    *   If the event is a release tag, the pipeline dynamically targets the `hub` GitHub environment and sets the container image tag to the release version.
    *   Otherwise, it targets the `bananas` environment and sets the container image tag to the Git commit SHA.
3.  **Rolling Updates:**
    *   Images are built and pushed to GitHub Container Registry (`ghcr.io`).
    *   The workflow executes `kubectl apply` on the Kubernetes templates.
    *   Kubernetes uses a `RollingUpdate` strategy, updating the pods one-by-one. This guarantees **zero-downtime** updates while replacing running processes with the new version.
    *   The workflow executes `kubectl rollout status` to verify that the new pods become healthy and ready. If an update fails, GKE automatically rolls back to the previous stable version.
