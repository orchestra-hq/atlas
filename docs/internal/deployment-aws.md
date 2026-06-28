# Deployment reference: AWS

How a team deploys Atlas into their own AWS account. This is a _reference topology_, not a product surface: Atlas ships binaries and a join flow; the cloud wiring below is deliberately boring, and that's the point.

## Stance on Terraform/IaC (decided 2026-06-12)

Atlas does **not** ship or support Terraform as part of the product. We will publish **reference IaC under `examples/`** (AWS Terraform first, M2) as documentation/marketing. The design bar that keeps this honest: **the entire AWS deployment should fit in ~100 lines of Terraform**, because the product absorbs the hard parts (join, discovery, health, routing). If the reference module grows past that, it's a signal the product DX has a gap — fix the product, not the module.

## Topology

```text
                        your engineers / your app (VPC or internet)
                                        │ https (443)
                              ANTHROPIC_BASE_URL=https://atlas.yourco.com
                                        │
   ┌─ VPC ──────────────────────────────▼────────────────────────────────────┐
   │  public subnets        ┌──────────────────┐    Route53 + ACM cert       │
   │                        │       ALB        │                             │
   │                        └────────┬─────────┘                             │
   │  private subnet         ┌───────▼──────────┐                            │
   │  (control plane)        │  atlas server    │  t3.medium, systemd,       │
   │                         │                  │  state on gp3 EBS          │
   │                         └───────▲──────────┘                            │
   │                                 │ outbound-only worker connections      │
   │  private subnets        ┌───────┴────────┐  ┌──────────────────┐        │
   │  (workers, no inbound   │  atlas worker  │  │  atlas worker    │  ...   │
   │   SG rules at all)      │  g6.12xlarge   │  │  g5.xlarge       │        │
   │                         │  4×L4 → vLLM   │  │  1×A10G → vLLM   │        │
   │                         └───────┬────────┘  └────────┬─────────┘        │
   │                                 │ NAT gw (egress: HF/S3 weights, server)│
   └─────────────────────────────────┼───────────────────────────────────────┘
                                     ▼
                   S3 weights mirror (optional) / Hugging Face
```

### Control plane

- **One small instance** (e.g. `t3.medium`) or an ECS/Fargate service. It's a single Go binary; systemd unit; no GPU.
- **State** is a single directory (SQLite + config) on gp3 EBS — snapshot it and you've backed up the platform. (Optional external Postgres is a possible later feature for HA; v1 is single-node control plane per ADR-0003.)
- **ALB in front**, ACM certificate, Route53 record (`atlas.yourco.com`). ALB health checks hit the server's `/healthz`. Security group: 443 from wherever your clients live — VPC-internal only is the common posture; the endpoint never needs to be public if your apps run in the same VPC/peered VPCs.

### Workers

- **GPU EC2 instances** sized to your models: `g6.xlarge` (1×L4 24GB) for 7–14B class, `g6.12xlarge`/`g5.12xlarge` (4 GPUs) for 70B-class with tensor parallel, `p4d/p5` only if you're serving very large models.
- **Zero inbound security group rules.** Workers dial out to the server (ADR-0003), so they sit in private subnets with NAT egress only. There is nothing to expose, nothing to firewall, no SSH required for operation.
- **Provisioning is user-data, not configuration management:**

  ```bash
  #!/bin/bash
  curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh
  atlas worker --join wss://atlas.yourco.com/workers/connect \
               --token "$(aws secretsmanager get-secret-value --secret-id atlas/join-token --query SecretString --output text)"
  ```

  That script _is_ the integration. Join token lives in Secrets Manager/SSM; instance role grants read on it.

- **Weights cache** on instance NVMe or gp3 EBS per worker, so each worker pulls a model once and cold boots are fast. (Weights are sourced from Hugging Face or an `https://` GGUF URL via the catalog; an S3 weights mirror to spare ten workers each pulling 140GB is a possible future source type, not yet implemented.)

### Scaling and spot

Because workers are stateless executors that self-register and are removed on heartbeat timeout:

- Put workers in an **Auto Scaling Group** per pool (per instance type/label). Scale out = new instance runs user-data and appears in the fleet in minutes. Scale in = instance gets SIGTERM, worker drains in-flight requests, disappears.
- **Spot instances work naturally** — an interrupted worker is just a heartbeat timeout; the scheduler re-places its model instances on remaining capacity. This is a big cost lever for GPU compute and worth marketing explicitly.
- v1 scaling is "you set ASG desired capacity." Atlas-driven autoscaling (scale signal from queue depth) is an M3 candidate, exposed as a metric ASGs can consume rather than Atlas calling EC2 APIs.

## Product requirements this topology imposes

These fall out of the reference deployment and are now design constraints (folded into architecture/roadmap):

1. **Non-interactive worker join** — flags/env only, no prompts (user-data compatibility). _(M1)_
2. **Graceful drain on SIGTERM** — finish/hand off in-flight requests before exit (ASG scale-in, spot interruption). _(M1)_
3. **Heartbeat-timeout worker removal** — vanished workers leave the pool automatically. _(M1)_
4. **`/healthz`** (liveness) and **`/readyz`** (serving traffic) endpoints on the server for LB health checks. _(M0)_
5. **Single-directory state** for trivial backup/restore. _(M0)_
6. **`s3://` (and `https://`) model sources** in the registry alongside Hugging Face. _(M2)_
7. **Prometheus metrics** including a queue-depth/utilization signal usable for external autoscaling. _(M2)_

## The same pattern elsewhere

Nothing above is AWS-specific: GCP is a MIG of L4 instances + the same user-data; Azure is a VMSS; bare metal is the systemd unit. And because workers dial out, hybrid is free — control plane in AWS, workers split across an on-prem DGX and a GCP pool, one endpoint. That hybrid story is a marketing asset; the reference docs should show it.
