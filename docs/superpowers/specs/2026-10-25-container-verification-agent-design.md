# Container verification agent

## Intent and scope

Run the Databasus backup verification agent as a container, using either a Docker socket or the Kubernetes API to create disposable PostgreSQL verification workloads. Add an independent Go module under `agent/verification-container/`. Leave `agent/verification/` unchanged.

Preserve the existing backend protocol and verification semantics: capacity heartbeats, claims, streamed downloads, archive owner roles, PostgreSQL restore, TimescaleDB hooks, statistics, cancellation, disk monitoring and result reporting. Support the same PostgreSQL major versions and image selection rules as the original agent. No backend or frontend changes are planned.

## Approaches

The user selected an independent implementation over extracting a shared core. Copy the relevant source and tests into the new module and adapt them there. Do not copy executables, daemon management, self-upgrade code or obsolete deployment fixtures. This keeps the original agent isolated, at the cost of maintaining duplicated verification logic. A shared core would reduce duplication but require broader changes to the original agent.

The user approved this independent implementation and the following deployment design.

## Process and configuration

Run a single foreground process that handles SIGTERM and SIGINT, stops claiming work, cancels active jobs, waits for cleanup and exits. Container image rollouts replace binary self-updates. Do not fork a daemon or store credentials in a generated configuration file.

Configure backend URL, existing agent ID and token, runtime selection, capacity limits, image repository and runtime settings through environment variables. Support a token file for mounted secrets. Require HTTPS unless an explicit insecure HTTP setting is enabled. Validate required settings and runtime connectivity before claiming jobs. Capacity is an explicit operator allocation for verification workloads, not the controller container's own CPU or memory limit.

Run one controller per agent ID. Kubernetes uses one replica and the Recreate deployment strategy. Docker examples use one service instance. Concurrent controllers sharing an agent ID are unsupported, matching the existing agent's single-owner assumption; document this constraint prominently.

## Runtime boundary

Keep orchestration independent of the runtime. Each runtime implements workload creation, command execution with streamed stdin and distinct stdout/stderr/exit status, PostgreSQL connection information, disk usage measurement, termination and startup cleanup. Preserve the distinction between infrastructure errors and a completed restore command that rejects an archive.

```text
Databasus backend
    | heartbeat / claim / stream / report
Container agent
    | runtime selection
    +-- Docker socket --> disposable PostgreSQL container
    +-- Kubernetes API --> disposable PostgreSQL pod
    |                         |
    +---- restore + SQL checks-+
```

Each job registers with the heartbeat before workload creation, starts PostgreSQL, streams its archive directly into the workload, compares the source SHA-256 checksum with the staged file, restores it, collects statistics, reports its outcome and removes the workload. A source that never reaches EOF or a mismatched checksum fails staging and uses the existing download retry policy; it must not reach restore or produce a corrupt-backup verdict. No decrypted archive is written to the controller filesystem.

## Docker runtime

Use the Docker SDK and a mounted Unix socket. Require an explicit controller container name or ID so the runtime can attach the controller to each job's private internal bridge network. Connect SQL checks to the workload's network address on port 5432; publish no host ports. Detach the controller when deleting the job network. Keep the controller's existing network for backend access.

Create a separate internal network per verification. Preserve non-root PostgreSQL execution, dropped capabilities, CPU/memory/PID limits and ephemeral storage removal. Label every owned resource with the new agent implementation, agent ID and verification ID, so cleanup cannot target the original agent's resources. Socket access gives the controller authority over the Docker daemon; document this deployment trust requirement.

## Kubernetes runtime

Use the official Go Kubernetes client with in-cluster credentials. Create one PostgreSQL pod per verification in a configured namespace. Stream archive writes and restore commands through the pod exec API, preferring WebSockets and negotiating SPDY only if the initial WebSocket upgrade is unavailable. The namespaced exec permission includes both `get` and `create` for these transports. Connect SQL checks directly to the ready pod IP; no Service, host networking, host ports, Docker socket or privileged pod is needed.

For commands receiving stdin, retain a shell parent until the child finishes so the exec session remains open while input is consumed. Pass commands and their arguments as separate argv entries and propagate the child exit code. Keep checksum verification independent of transport success.

Set CPU and memory limits from the per-job allocation and requests to 10% of those limits, rounding memory requests up to a whole byte. Advertise the full configured capacity to Databasus. Keep ephemeral-storage requests equal to their configurable limits. Mount disk-backed emptyDir volumes for PostgreSQL data and archive staging, with an explicit PGDATA path compatible with supported images. Continue measuring actual disk usage against the backend's per-job budget. Storage requests and emptyDir size limits supplement the existing watcher; neither guarantees an immediate filesystem quota on every cluster.

Verification pods run as a non-root PostgreSQL user, drop all capabilities, disable privilege escalation, use the runtime-default seccomp profile and do not mount service account credentials. Use image-compatible volume ownership. The controller's namespaced Role grants only the pod and pod-exec operations the implementation actually needs; no cluster-wide permission or secret-reading permission is planned.

Ship NetworkPolicy manifests that isolate verification pods, allow PostgreSQL ingress from the controller and deny verification-pod egress. The deployment documentation states that a NetworkPolicy-capable CNI is required for these controls. The controller retains access to the backend and Kubernetes API. Private registries use an optional configured imagePullSecrets reference.

## Lifecycle and recovery

Use bounded startup/readiness and cleanup deadlines. Remove partially created resources on failure using a context independent of a cancelled job. Surface image-pull, scheduling and readiness failures as setup failures rather than corrupt-backup verdicts. Keep cancellation and report retry semantics consistent with the original agent.

At startup, remove only resources carrying this implementation's ownership labels and the configured agent ID. Kubernetes verification pods also have an owner reference to the controller pod so garbage collection removes them after controller replacement. Documentation covers manual cleanup if the Docker controller is killed before cleanup. Retry cleanup safely when resources are already absent.

## Deliverables and verification

Include the Go module, a multi-stage Dockerfile with pinned base images, a Compose example, Kubernetes Deployment/ServiceAccount/Role/RoleBinding/NetworkPolicy examples, and a README with configuration, resource sizing, deployment, upgrade and cleanup instructions. Keep all implementation-specific documentation in the new folder.

Preserve and adapt relevant existing behavior tests. Add runtime tests for resource ownership, security settings, direct networking, streamed exec, exit status, cancellation, partial failures and cleanup. Test Kubernetes operations with the official fake client where suitable, and test exec transport separately. Run formatting, lint, Go tests and a Linux build for the new module. Validate deployment manifests with available tools. Run real Docker and Kubernetes smoke tests if those environments are available; otherwise clearly report those checks as unverified and provide reproducible commands.

## Remaining implementation choices

The builder chooses internal package names, pinned compatible dependency versions, exact environment variable names, sensible configurable timeout defaults and test fixture organization. No automatic cluster autoscaling, new database engines, Helm chart, operator, backend API changes or deployment to the user's infrastructure is included.
