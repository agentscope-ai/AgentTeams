# Runtime monitoring API

Both endpoints require a Controller administrator or Manager identity. Scoped
Humans, Leaders and Workers are denied. Responses use `Cache-Control: no-store`.
The API resolves Worker/Manager resources in the Controller namespace and scope;
it does not accept arbitrary host container names or Docker paths.

## GET /api/v1/monitoring

Query: `page` (default 1), `limit` (default 12, maximum 50), `search` (optional,
case-insensitive name/kind/runtime/phase substring). Invalid page/limit returns
400. Out-of-range pages are clamped to the last page.

The response contains `mode`, `sampledAt`, `items`, `total`, `page`, `limit`,
and `totalPages`. Only the requested page is sampled, with at most six concurrent
Docker reads and an eight-second request deadline. Each item contains:

- `kind`: `worker`, `manager`, or `component` (`controller` / `dashboard`).
- `name`, optional runtime/model/phase, and Worker activity/task metadata.
- `availability`: `available`, `unsupported`, `missing`, or `error`; `reason`
  explains an unavailable source without exposing upstream response bodies.
- `metrics`: container state, startedAt, restartCount, oomKilled, exitCode,
  cpuPercent, memoryUsageBytes, memoryLimitBytes, sampledAt, optional usageError.

CPU is a sampled Docker counter delta: 100% means one fully occupied CPU core,
so multicore usage may exceed 100%. A missing/reset baseline produces null.
Memory excludes inactive file cache (cgroup v1/v2). Only an explicit Docker
HostConfig memory limit is returned; null means no explicit container quota,
not a quota equal to the host's available memory. Stopped containers have
lifecycle information and null live usage. A stats failure preserves inspect
data and sets usageError; it does not manufacture zero usage.

This version supplies Docker embedded deployments. Externally managed Workers
and Kubernetes/other deployments without this data source return unsupported
items with null metrics. The embedded Controller container also includes its
Matrix/gateway/storage services; their usage is not attributed separately.
There is no historical metric storage or process-level attribution.

## GET /api/v1/monitoring/{kind}/{name}/logs

Query: `tail` (default 300, range 1–1000), `since` (optional RFC3339 timestamp).
The response contains `kind`, `name`, `tail`, `since`, `sampledAt`, `text`, and
`truncated`. stdout/stderr preserve Docker delivery order; TTY and multiplexed
streams are supported. The source is container output, not every application
log file or an archived log store.

Reads are bounded to 1 MiB and eight seconds, with truncation reported explicitly.
Common credential assignments and Bearer/Basic authorization values are masked;
this is best-effort masking, not a guarantee that arbitrary application output
contains no sensitive data. Unknown resources and missing containers return
404, unsupported sources 501, and source failures 502. No exec, restart, or
configuration writes are performed. CLI/Manager skill integration is separate.
