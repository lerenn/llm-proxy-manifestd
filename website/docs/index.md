---
layout: home
hero:
  name: LLM Proxy ManifestD
  text: A headless proxy for shared AI accounts
  tagline: "Per-account concurrency limits and priority queues for parallel AI coding sessions. Built for Kubernetes: deployed with Helm, configured by a hot-reloading ConfigMap, with optional CLI sidecars. No database."
  actions:
    - theme: brand
      text: Get Started
      link: /guide/getting-started
    - theme: alt
      text: Architecture
      link: /guide/architecture
features:
  - title: IaC-first
    details: One YAML manifest, shipped as a Kubernetes ConfigMap and installed with Helm. Nothing to click.
  - title: Priority queues
    details: Interactive sessions jump ahead of background agents. Optional aging prevents starvation.
  - title: Shared across pods
    details: Optional Valkey coordination shares slot limits and the priority queue, so rolling updates never double the limit.
  - title: Zero-buffer streaming
    details: SSE responses are flushed as they arrive, and each slot is held until the stream ends.
  - title: Safe hot reloads
    details: ConfigMap changes are applied live. Invalid configs are rejected, in-flight streams are never dropped.
---
