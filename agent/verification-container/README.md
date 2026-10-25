# Container verification agent

This independent agent verifies PostgreSQL logical backups using disposable Docker containers or Kubernetes pods. It uses the existing Databasus verification-agent credentials and backend protocol. The original implementation in `../verification/` is unchanged.

Run exactly one controller per agent ID. Never share an ID between this agent, the original agent or another controller. Startup removes this implementation's resources for its agent ID; concurrent owners could delete each other's running work. The Kubernetes example uses one replica and a Recreate rollout.

The controller runs in the foreground, sends capacity heartbeats, claims backups, streams each archive directly into a verification workload, verifies its SHA-256 checksum, restores it, checks the database and reports results. Incomplete or mismatched transfers are retried before restore and are reported as infrastructure failures if retries are exhausted. SIGTERM cancels active work and waits for cleanup. Upgrade the container image to upgrade the agent; it does not replace its own binary.

## Configuration

| Environment variable | Meaning |
| --- | --- |
| `DATABASUS_URL` | Required backend URL. HTTPS is required by default. |
| `AGENT_ID` | Required existing backend verification agent ID. |
| `AGENT_TOKEN` | Agent credential, mutually exclusive with `AGENT_TOKEN_FILE`. |
| `AGENT_TOKEN_FILE` | Path to a readable mounted credential file. Trailing whitespace is trimmed. |
| `VERIFICATION_RUNTIME` | `docker` (default) or `kubernetes`. |
| `MAX_CPU` | Required total CPU allocation for verification workloads. |
| `MAX_RAM_MB` | Required total RAM allocation in MiB for verification workloads. |
| `MAX_DISK_GB` | Required advertised disk capacity in GiB. |
| `MAX_CONCURRENT_JOBS` | Required positive concurrency limit. Each job must receive at least one CPU and 512 MiB. |
| `VERIFICATION_PG_IMAGE_REPO` | PostgreSQL image repository, default `databasus/verification-postgres`. Tags come from the assigned PostgreSQL major. |
| `ALLOW_INSECURE_HTTP` | Set to `true` only when accepting unencrypted backend credentials and backup streams. |
| `DOCKER_CONTROLLER_CONTAINER` | Required in Docker mode: this controller's Docker container name or ID. |
| `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH` | Standard Docker SDK connection settings; default is the local Unix socket. |
| `KUBERNETES_NAMESPACE` | Required in Kubernetes mode: namespace containing both controller and verification pods. |
| `KUBERNETES_POD_NAME`, `KUBERNETES_POD_UID` | Required controller identity, normally supplied through the downward API. |
| `KUBERNETES_STORAGE_GB` | Per-verification pod ephemeral storage request/limit in GiB; defaults to `MAX_DISK_GB`. |
| `KUBERNETES_IMAGE_PULL_SECRETS` | Optional comma-separated names of existing registry secrets in the verification namespace. |

Allocate verification CPU, RAM and disk separately from the controller's own resources. Leave node capacity for the controller, operating system and runtime. With concurrency greater than one, size `KUBERNETES_STORAGE_GB` per pod so all concurrent requests fit the cluster and match the capacity advertised to Databasus. The backend's assignment controls the per-job disk budget; the agent monitors the archive and database footprint and cancels work that exceeds it.

Kubernetes verification pods request 10% of their CPU and memory limits, with memory rounded up to a whole byte. For example, a job limited to 1 CPU and 512 MiB requests `100m` CPU and approximately 51.2 MiB memory. Limits and the capacity advertised to Databasus still use the full configured allocation. Ephemeral-storage requests remain equal to their limits. These percentages apply to generated verification pods; the controller's resources are configured separately in the Deployment.

PostgreSQL majors 12–18 and exact version-matched TimescaleDB images follow the original agent's image selection and restore rules. The bundled PostgreSQL repository falls back to stock `postgres` when an image is unavailable; TimescaleDB never falls back. Custom Kubernetes PostgreSQL images must support the configured non-root UID (999 for PostgreSQL, 70 for TimescaleDB), the standard entrypoint, `pg_isready`, `pg_restore`, `du`, `dd`, `sha256sum`, `env` and `sh`.

## Docker

Build from this directory:

```sh
docker build -t databasus-verification-container-agent:local .
```

Create an agent through Databasus and save its token in a local file readable by UID 65532. Export the settings without placing the token itself in shell history:

```sh
export DATABASUS_URL=https://databasus.example.com
export AGENT_ID=your-agent-id
export AGENT_TOKEN_FILE=/secure/path/agent-token
export DOCKER_SOCKET_GID=$(stat -c '%g' /var/run/docker.sock)
docker compose config --quiet
docker compose up -d
docker compose logs -f
```

### Configure Compose with a `.env` file

You can save Compose settings in `.env` beside `compose.yaml` instead of exporting them in each shell:

```dotenv
DATABASUS_URL=https://databasus.example.com
AGENT_ID=your-agent-id
AGENT_TOKEN_FILE=./agent-token.txt
DOCKER_SOCKET_GID=0
MAX_CPU=1
MAX_RAM_MB=2048
MAX_DISK_GB=20
MAX_CONCURRENT_JOBS=1
VERIFICATION_PG_IMAGE_REPO=databasus/verification-postgres
ALLOW_INSECURE_HTTP=false
```

Save only the agent token in `agent-token.txt` and keep both files out of version control. Here `AGENT_TOKEN_FILE` is a host file path used by Compose. Compose mounts that file at `/run/secrets/agent_token` and supplies the container's path automatically. Use a different agent ID and token for each concurrently running controller.

On Linux, replace `DOCKER_SOCKET_GID=0` with the result of `stat -c '%g' /var/run/docker.sock`. On Windows, use Docker Desktop with Linux containers enabled. The Linux controller uses the same `/var/run/docker.sock` mount shown in `compose.yaml`; no Windows named-pipe mount is needed. Docker Desktop's socket group was verified as `0` in the tested environment. To check yours from either PowerShell or Git Bash:

```sh
docker run --rm --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock golang:1.26.3-bookworm stat -c %g /var/run/docker.sock
```

Run these commands from the directory containing `compose.yaml` and `.env`:

```sh
docker compose --env-file .env config --quiet
docker compose --env-file .env up -d
docker compose logs -f
```

If Git Bash still reports that `DOCKER_SOCKET_GID` is missing, an empty exported shell variable can override the `.env` value. Clear it before running Compose:

```sh
unset DOCKER_SOCKET_GID
docker compose --env-file .env config --quiet
docker compose --env-file .env up -d
```

The equivalent PowerShell command to clear that override is `Remove-Item Env:DOCKER_SOCKET_GID -ErrorAction SilentlyContinue`. Also check that Windows saved the file as `.env`, rather than `.env.txt`.

### Use a published controller image

To run a published image, remove `build: .` from `compose.yaml` and replace its `image` value with your registry image, for example `zimbres/databasus-verification-container:0.4`. Then run:

```sh
docker compose --env-file .env pull
docker compose --env-file .env up -d
```

### Docker access and upgrades

Socket access gives the controller control over the Docker daemon and its host. Use a dedicated verification host and trusted controller image. The controller itself runs as UID 65532; `group_add` grants access to the socket without privileged mode. Do not mount the socket into PostgreSQL workloads.

Each verification gets a private internal bridge network. The controller joins that network for SQL checks and detaches during cleanup. Database ports are never published on the host. Keep the controller on its normal Compose network so it can reach Databasus. The configured controller identity must identify this container on the same daemon that creates the verification containers.

For a locally built image:

```sh
docker compose stop
docker compose build --pull
docker compose up -d
```

For a published image, stop the controller, pull the replacement image and run `docker compose up -d`.

Keep the same agent ID and stop the old controller before starting its replacement. Allow at least the example's 90-second shutdown grace period.

## Kubernetes

Use a dedicated namespace and a CNI that enforces NetworkPolicy. Verification pods have no service account token, host networking, privileged mode or Docker socket. They run as non-root, drop capabilities, use the runtime-default seccomp profile and accept PostgreSQL connections only from the controller. Their NetworkPolicy denies egress. The controller needs access to the Kubernetes API and Databasus.

Build and publish the image to your registry, then replace the Deployment image in `deploy/kubernetes.yaml` with your immutable image reference and set `DATABASUS_URL`. The example image name is local and must be loaded into every eligible node or replaced with a reachable registry image.

The manifest includes the namespace and an Opaque Secret named `verification-agent` with the `agent-id` and `token` keys already referenced by the Deployment. Replace `REPLACE_WITH_AGENT_ID` and `REPLACE_WITH_AGENT_TOKEN` with credentials from Databasus before applying it. Keep the populated manifest out of version control. Reapplying the manifest updates the Secret, so preserve those values in your deployment configuration.

```sh
kubectl apply -f deploy/kubernetes.yaml
kubectl -n databasus-verification rollout status deployment/verification-controller
kubectl -n databasus-verification logs -f deployment/verification-controller
```

The controller uses in-cluster authentication and a namespaced Role for pod creation, inspection, deletion and exec. RBAC cannot constrain pod creation by labels, so this controller can create pods throughout its namespace. Keep unrelated workloads and credentials out of that namespace. The controller requires no permission to read Secrets; kubelet mounts its credential. Configure registry pull secrets separately for the controller image and use `KUBERNETES_IMAGE_PULL_SECRETS` for verification images.

Exec uses WebSocket streaming, matching modern `kubectl`, and negotiates SPDY only when the WebSocket upgrade is unavailable. The controller Role therefore requires both `get` and `create` on `pods/exec`. Apply the updated Role when upgrading the controller. Checksum verification performs an additional sequential read of the staged archive inside the workload; plaintext remains out of the controller filesystem.

Commands receiving stdin run under a shell parent that stays alive until the child finishes. This avoids silent truncation observed with direct exec uploads on affected Kubernetes runtimes. Command arguments remain separate argv entries, and the child's exit code is preserved. Uploads still require EOF and a matching checksum before restore.

Pods use disk-backed emptyDir volumes and ephemeral-storage requests/limits. Filesystem quota enforcement depends on the cluster; the disk watcher remains necessary. Kubernetes limits PIDs through node/runtime configuration rather than a per-pod field. Configure the node's pod PID limit before accepting untrusted restores. Scheduling or image failures are agent setup failures, not proof that a backup is corrupt.

Verification pods reference their controller pod as owner. Kubernetes garbage collection removes them after controller deletion; startup also removes owned leftovers. Roll out image changes through the Deployment, keeping the Recreate strategy and one replica. A new controller can begin only after the prior controller has stopped.

## Recovery and cleanup

Resources carry `databasus.verification.implementation=container-agent`, the agent ownership label and a verification ID. These labels distinguish them from the original agent. Kubernetes encodes the agent ID as the first 40 hex characters of its SHA-256 hash to support arbitrary IDs in label values.

Stop the controller before manual cleanup. For Docker, list resources for the exact ID, remove only the selected containers with their anonymous volumes, then remove the selected networks:

```sh
docker ps -a --filter label=databasus.verification.implementation=container-agent \
  --filter label=databasus.verification.agent_id=your-agent-id
docker network ls --filter label=databasus.verification.implementation=container-agent \
  --filter label=databasus.verification.agent_id=your-agent-id
```

Use `docker rm -fv <selected-container-id>` and `docker network rm <selected-network-id>`. If a stopped controller is still attached, disconnect it with `docker network disconnect -f <selected-network-id> <controller-id>` first. Do not prune the daemon.

For Kubernetes, calculate the agent label locally and inspect before deleting:

```sh
printf %s your-agent-id | sha256sum | cut -c1-40
kubectl -n databasus-verification get pods \
  -l 'databasus.verification.implementation=container-agent,databasus.verification.agent_id=<hash>'
```

Delete only the selected verification pods. A forced controller kill can delay cleanup; backend ownership recovery and the next controller startup handle interrupted assignments.

## Development and verification

Use the repository-pinned Go version. From this directory:

```sh
go test ./... -count=1
go test -race ./... -count=1
just format
just lint
just build
```

Run the Docker integration smoke test from a disposable toolchain container (it creates and removes labeled PostgreSQL workloads):

```sh
docker run --rm --name databasus-verification-smoke \
  -e DOCKER_CONTROLLER_CONTAINER=databasus-verification-smoke \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD:/workspace" -w /workspace \
  golang:1.26.3-bookworm \
  go test -tags=integration ./e2e -count=1 -v
```

The default runtime tests use the official Kubernetes fake client and isolated Docker SDK seams. They do not prove cluster scheduling, network policy enforcement or registry access.

An optional live upload regression sends synthetic binary data through the real Kubernetes client. It requires an explicitly selected disposable pod with a `postgres` container, writable `/tmp`, and the label `databasus.verification.implementation=upload-diagnostic`. It uses the selected kubeconfig, writes only diagnostic files in that pod, and does not create or delete the pod:

```sh
KUBE_UPLOAD_NAMESPACE=your-test-namespace KUBE_UPLOAD_PROBE=your-probe-pod \
  go test -tags=integration ./internal/features/kubernetes \
  -run Test_Exec_WhenLiveProbeConfigured_PreservesBinaryUpload -count=1
```

A live deployment smoke test should cover both runtimes:

1. Assign a known PostgreSQL backup with a table containing rows. Confirm Databasus reports completion and correct table statistics, then confirm the temporary workload disappears.
2. Assign a truncated custom-format archive. Confirm a failed restore is reported and the workload disappears.
3. Cancel a verification during download or restore. Confirm it stays cancelled in Databasus and resources disappear.
4. Restart the controller during a verification. Confirm owned leftovers are removed and a subsequent verification succeeds. Resources belonging to another agent must remain.

Do not use an existing production cluster as an automatic test fixture. Run deployment smoke checks in a disposable cluster or explicitly chosen test namespace.
