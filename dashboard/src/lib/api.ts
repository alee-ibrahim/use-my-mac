// Typed client for the router's REST API. The dashboard authenticates with a
// same-site session cookie; writes carry X-UMM-CSRF so cross-site requests
// can't reuse the cookie.

export type Telemetry = {
  collected_at: string
  console_user: string
  user_idle_seconds: number
  screen_locked: boolean
  cpu_count: number
  user_cpu_percent: number
  our_cpu_percent: number
  user_mem_bytes: number
  our_mem_bytes: number
  mem_total_bytes: number
  mem_available_bytes: number
  memory_pressure: string
  thermal: string
  on_ac: boolean
  battery_percent: number
  gpu_util_percent: number
  disk_free_bytes: number
  chip: string
  model: string
  gpu_cores: number
  os: string
}

export type Mac = {
  id: string
  name: string
  online: boolean
  last_seen?: string
  agent_state: string
  drain: boolean
  opted_out: boolean
  quarantined_until?: string
  runtimes: string[]
  // Allowlisted runtimes this Mac cannot run (program missing, not executable,
  // or a task could not start it). Tasks needing them are not placed here.
  unavailable_runtimes?: UnavailableRuntime[]
  // Effective labels: the agent's own (agent_labels) with the router-side
  // labels an admin set (admin_labels) taking precedence.
  labels: Record<string, string>
  agent_labels?: Record<string, string>
  admin_labels?: Record<string, string>
  reservation?: MacReservation
  // Effective concurrency; max_tasks_set is the admin's setting (0 = the
  // agent's own) and agent_max_tasks the agent's hard limit.
  max_tasks: number
  max_tasks_set?: number
  agent_max_tasks?: number
  running: string[]
  telemetry?: Telemetry
  version?: string
  update?: MacUpdate
  created_at?: string
  // Why the agent is in its state (e.g. why it is unhealthy).
  state_reason?: string
  isolation?: Isolation
}

// A Mac reserved for some owners only runs their tasks.
export type MacReservation = {
  owners?: string[]
  owner_names?: Record<string, string>
  until?: string
  note?: string
  by?: string
  mine: boolean
}

export type MacReservationRequest = {
  owners: string[]
  hours?: number
  note?: string
}

// How a Mac keeps tasks away from its users' data.
export type Isolation = {
  work_volume?: string // "dedicated" or "system"
  work_volume_note?: string
  network?: string // "allow" or "deny"
  home_dirs?: string // "ok", "exposed" or "unchecked"
  work_space_bytes: number
  work_capacity_bytes: number
  system_free_bytes?: number
  reserve_bytes?: number
}

export type UnavailableRuntime = {
  name: string
  program?: string
  reason: string
  source?: string // "preflight" or "exec"
  since?: string
}

export type MacUpdate = {
  state?: string
  from_version?: string
  to_version?: string
  message?: string
  at?: string
  checked_at?: string
  desired?: string
}

export type UpdateEvent = {
  at: string
  state: string
  from_version?: string
  to_version?: string
  message?: string
}

export type UpdateSettings = {
  paused: boolean
  target_version: string
  canary_version: string
  canary_percent: number
  canary_macs: string[]
  drain_timeout_seconds: number
}

export type UpdatesInfo = {
  settings: UpdateSettings
  router_version: string
  relay: boolean
  release_url?: string
}

export type TaskSpec = {
  key: string
  runtime: string
  args?: string[]
  depends_on?: string[]
  resources?: {
    ram_mb?: number
    gpu?: boolean
    expected_seconds?: number
    labels?: Record<string, string>
  }
  outputs?: { name: string }[]
  macs?: string[]
  prefer?: { labels?: Record<string, string>; macs?: string[]; bonus?: number }
}

export type Cleanup = {
  after_download?: boolean
  after_hours?: number
  keep_inputs?: boolean
  keep_outputs?: boolean
  delete_job?: boolean
}

export type Attempt = {
  attempt: number
  agent_id: string
  agent_name?: string
  started_at: string
  finished_at?: string
  outcome: string
  error?: string
  score: number
}

export type Task = {
  id: string
  job_id: string
  key: string
  spec: TaskSpec
  state: string
  attempt: number
  failures: number
  agent_id?: string
  error?: string
  started_at?: string
  finished_at?: string
  attempts?: Attempt[]
  outputs_done?: string[]
  // Live progress: phase is queued, starting, downloading, running,
  // uploading or done; percent is the task's overall 0-100.
  phase?: string
  percent?: number
  progress?: TaskProgress
  // Why a pending task has no Mac yet (reserved Macs, pinned Macs).
  waiting?: string
  placement?: {
    id: string
    ok: boolean
    reason?: string
    score: number
    parts?: Record<string, number>
  }[]
}

export type Job = {
  id: string
  name: string
  owner: string
  state: string
  created_at: string
  finished_at?: string
  counts: Record<string, number>
  // Overall progress (0-100) including running tasks' live progress.
  percent?: number
  phase?: string
  tasks?: Task[]
  cleanup?: Cleanup
  cleaned_at?: string
}

// Latest live report from the Mac running a task. percent is within the
// phase, -1 when unknown.
export type TaskProgress = {
  attempt: number
  phase: string
  percent: number
  bytes_done?: number
  bytes_total?: number
  bytes_per_sec?: number
  message?: string
  updated_at?: string
}

export type APIKey = {
  id: string
  name: string
  role: string
  prefix: string
  created_at: string
  last_used_at?: string
  revoked_at?: string
  secret?: string
}

export type Me = { subject: string; name: string; role: string; via: string }

export type Settings = {
  weights: Record<string, number>
  limits: {
    idle_full_seconds: number
    min_disk_free_mb: number
    allow_battery: boolean
    min_battery_percent: number
  }
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown
): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: {
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      ...(method !== "GET" ? { "X-UMM-CSRF": "1" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    let msg = res.statusText
    try {
      const j = await res.json()
      if (j.error) msg = j.error
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg)
  }
  if (res.status === 204) return undefined as T
  const ct = res.headers.get("content-type") ?? ""
  return (
    ct.includes("application/json") ? res.json() : res.text()
  ) as Promise<T>
}

// How the dashboard may be signed in to. apikey = API-key sign-in allowed.
export type AuthConfig = {
  oidc: boolean
  apikey: boolean
  auto_sign_in: boolean
}

// Starts single sign-on and returns to `ret` (sanitised again by the server).
export function ssoURL(ret = "/") {
  return `/auth/login?return=${encodeURIComponent(ret)}`
}

export type RuntimeMacStatus = {
  id: string
  name: string
  state: string
  version?: string
  error?: string
}

export type RuntimeCatalogEntry = {
  package: {
    name: string
    version: string
    description?: string
    commands: Record<string, string[]>
    artifacts: { os: string; arch: string; format: string; size?: number }[]
  }
  key_id: string
  prewarm: boolean
  published_at: string
  published_by?: string
  macs?: RuntimeMacStatus[]
}

export type RuntimeCatalog = { revision: number; packages: RuntimeCatalogEntry[] }

export const api = {
  authConfig: () => request<AuthConfig>("GET", "/auth/config"),
  loginWithKey: (key: string) => request<Me>("POST", "/auth/apikey", { key }),
  logout: () => request<void>("POST", "/auth/logout"),
  me: () => request<Me>("GET", "/v1/me"),
  macs: () => request<Mac[]>("GET", "/v1/macs"),
  patchMac: (
    id: string,
    patch: {
      drain?: boolean
      opted_out?: boolean
      name?: string
      max_tasks?: number
    }
  ) => request<void>("PATCH", `/v1/macs/${id}`, patch),
  setMacLabels: (id: string, labels: Record<string, string>) =>
    request<void>("PUT", `/v1/macs/${id}/labels`, { labels }),
  reserveMac: (id: string, r: MacReservationRequest) =>
    request<void>("PUT", `/v1/macs/${id}/reservation`, r),
  unreserveMac: (id: string) =>
    request<void>("DELETE", `/v1/macs/${id}/reservation`),
  deleteMac: (id: string) => request<void>("DELETE", `/v1/macs/${id}`),
  enrolToken: (ttl_hours: number) =>
    request<{ token: string; expires_at: string }>(
      "POST",
      "/v1/enrolment-tokens",
      { ttl_hours }
    ),
  jobs: (state = "", limit = 200) =>
    request<Job[]>(
      "GET",
      `/v1/jobs?limit=${limit}${state ? `&state=${state}` : ""}`
    ),
  job: (id: string) => request<Job>("GET", `/v1/jobs/${id}`),
  cancelJob: (id: string) => request<Job>("POST", `/v1/jobs/${id}/cancel`),
  task: (id: string) => request<Task>("GET", `/v1/tasks/${id}`),
  logs: (id: string) => request<string>("GET", `/v1/tasks/${id}/logs`),
  keys: () => request<APIKey[]>("GET", "/v1/api-keys"),
  createKey: (name: string, role: string) =>
    request<APIKey>("POST", "/v1/api-keys", { name, role }),
  revokeKey: (id: string) => request<void>("DELETE", `/v1/api-keys/${id}`),
  runtimes: () => request<RuntimeCatalog>("GET", "/v1/runtimes"),
  setRuntimePrewarm: (name: string, prewarm: boolean) =>
    request<void>("PUT", `/v1/runtimes/${encodeURIComponent(name)}/prewarm`, { prewarm }),
  deleteRuntime: (name: string) => request<void>("DELETE", `/v1/runtimes/${encodeURIComponent(name)}`),
  settings: () => request<Settings>("GET", "/v1/settings"),
  saveSettings: (s: Settings) => request<Settings>("PUT", "/v1/settings", s),
  updates: () => request<UpdatesInfo>("GET", "/v1/updates"),
  saveUpdates: (s: UpdateSettings) =>
    request<UpdatesInfo>("PUT", "/v1/updates", s),
  macUpdates: (id: string) =>
    request<UpdateEvent[]>("GET", `/v1/macs/${id}/updates`),
  installProfiles: () =>
    request<InstallProfile[]>("GET", "/v1/install/profiles"),
  installTokens: () => request<InstallToken[]>("GET", "/v1/install-tokens"),
  createInstallToken: (r: InstallTokenRequest) =>
    request<InstallToken>("POST", "/v1/install-tokens", r),
  revokeInstallToken: (id: string) =>
    request<void>("DELETE", `/v1/install-tokens/${id}`),
  installEvents: (id: string) =>
    request<InstallEvent[]>("GET", `/v1/install-tokens/${id}/events`),
  access: () => request<AccessPolicy>("GET", "/v1/access"),
  putAccessRule: (r: AccessRuleRequest) =>
    request<AccessRule>("POST", "/v1/access/rules", r),
  deleteAccessRule: (id: string) =>
    request<void>("DELETE", `/v1/access/rules/${encodeURIComponent(id)}`),
  setAccessDefault: (role: string) =>
    request<AccessPolicy>("PUT", "/v1/access/default", { role }),
  setGroupName: (id: string, name: string) =>
    request<void>("PUT", `/v1/access/groups/${encodeURIComponent(id)}`, {
      name,
    }),
  accessUsers: () => request<AccessUser[]>("GET", "/v1/access/users"),
  setUserRole: (subject: string, role: string) =>
    request<void>(
      "PUT",
      `/v1/access/users/${encodeURIComponent(subject)}/role`,
      { role }
    ),
  accessAudit: () =>
    request<AccessEvent[]>("GET", "/v1/access/audit?limit=200"),
  docs: () => request<DocInfo[]>("GET", "/v1/docs"),
  // Operations: metrics history, activity log, retention, cleanup.
  summary: () => request<Summary>("GET", "/v1/metrics/summary"),
  fleetSeries: (range: string, step = 0) =>
    request<Series>(
      "GET",
      `/v1/metrics/fleet?range=${range}${step ? `&step=${step}` : ""}`
    ),
  macSeries: (id: string, range: string) =>
    request<Series>(
      "GET",
      `/v1/metrics/macs/${encodeURIComponent(id)}?range=${range}`
    ),
  macSparks: (window = "1h") =>
    request<MacSparks>("GET", `/v1/metrics/macs?window=${window}`),
  macAttempts: (id: string, limit = 100) =>
    request<MacAttempt[]>(
      "GET",
      `/v1/macs/${encodeURIComponent(id)}/attempts?limit=${limit}`
    ),
  jobTimeline: (id: string) =>
    request<TimelineAttempt[]>(
      "GET",
      `/v1/jobs/${encodeURIComponent(id)}/timeline`
    ),
  events: (
    q: { kind?: string; target?: string; limit?: number; before?: string } = {}
  ) => {
    const p = new URLSearchParams()
    if (q.kind) p.set("kind", q.kind)
    if (q.target) p.set("target", q.target)
    if (q.before) p.set("before", q.before)
    p.set("limit", String(q.limit ?? 50))
    return request<OpsEvent[]>("GET", `/v1/events?${p}`)
  },
  retention: () => request<Retention>("GET", "/v1/retention"),
  saveRetention: (r: Retention) =>
    request<Retention>("PUT", "/v1/retention", r),
  deleteKey: (id: string) => request<void>("POST", `/v1/api-keys/${id}/purge`),
  purgeKeys: () => request<PurgeResult>("POST", "/v1/api-keys/purge"),
  deleteInstallToken: (id: string) =>
    request<void>("POST", `/v1/install-tokens/${id}/purge`),
  purgeInstallTokens: () =>
    request<PurgeResult>("POST", "/v1/install-tokens/purge"),
  // Deletes a job (cancelling it first if it runs); inputs also deletes the
  // input files its owner uploaded, unless another unfinished job uses them.
  deleteJob: (id: string, inputs = false) =>
    request<void>("DELETE", `/v1/jobs/${id}${inputs ? "?inputs=true" : ""}`),
  purgeJobs: (older_than_days: number, states?: string[]) =>
    request<PurgeResult>("POST", "/v1/jobs/purge", { older_than_days, states }),
  doc: (slug: string) =>
    request<string>("GET", `/v1/docs/${encodeURIComponent(slug)}`),
}

export function bytes(n: number | undefined): string {
  if (!n) return "–"
  const gb = n / 2 ** 30
  return gb >= 1 ? `${gb.toFixed(1)} GB` : `${(n / 2 ** 20).toFixed(0)} MB`
}

export function since(iso?: string): string {
  if (!iso) return "–"
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return `${Math.round(s)}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return `${Math.round(s / 86400)}d ago`
}

export function duration(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`
  return `${(seconds / 3600).toFixed(1)}h`
}

// One-line install.
export type InstallProfile = {
  name: string
  labels?: Record<string, string>
  runtimes: Record<string, string[]>
  max_tasks: number
  priority: string
  service_label: string
}
export type InstallTokenRequest = {
  profile: string
  labels?: Record<string, string>
  ttl_minutes: number
  max_uses: number
  note?: string
}
export type InstallToken = {
  id: string
  prefix: string
  profile: InstallProfile
  note?: string
  created_by?: string
  created_at: string
  expires_at: string
  max_uses: number
  uses: number
  revoked_at?: string
  last_used_at?: string
  state: string
  token?: string
  command?: string
}
export type InstallEvent = {
  at: string
  kind: string
  ip?: string
  agent_id?: string
  detail?: string
}

// An embedded documentation page (public, served at /docs).
export type DocInfo = { slug: string; title: string; path: string }

// Dashboard access: role rules for single-sign-on users.
export type AccessRule = {
  id: string
  kind: "group" | "user"
  value: string
  role: "admin" | "viewer"
  name?: string
  source: "config" | "ui"
  locked: boolean
  created_by?: string
  created_at?: string
  updated_at?: string
}
export type AccessRuleRequest = {
  kind: string
  value: string
  role: string
  name?: string
}
export type AccessPolicy = {
  default_role: string
  config_default_role: string
  ui_default_role: string
  rules: AccessRule[]
  group_names: Record<string, string>
  oidc: boolean
}
export type AccessUser = {
  subject: string
  name: string
  email?: string
  upn?: string
  oid?: string
  groups: string[]
  first_login: string
  last_login: string
  role: string
  reason: string
  rule_id?: string
  source?: string
}
export type AccessEvent = {
  at: string
  actor: string
  action: string
  detail: string
}

// Metrics history. Points carry "t" (bucket start, Unix ms) and, when the
// bucket has data, the fields below.
export type Point = { t: number } & Partial<Record<string, number>>
export type Series = {
  from: string
  to: string
  step_seconds: number
  resolution_seconds: number
  points: Point[]
}
export type MacSparks = {
  from: string
  to: string
  step_seconds: number
  macs: Record<string, Point[]>
}
export type Window = {
  succeeded: number
  failed: number
  timeout: number
  yielded: number
  lost: number
  cancelled: number
  assigned: number
  compute_hours: number
  success_rate: number // -1 when nothing finished
}
export type Summary = {
  at: string
  macs: Record<
    "total" | "online" | "busy" | "idle" | "yielded" | "draining" | "offline",
    number
  >
  tasks: Record<"running" | "queued" | "blocked", number>
  jobs: Record<"queued" | "running", number>
  last_24h: Window
  last_7d: Window
  router_version: string
  history_start?: string
}
export type OpsEvent = {
  id: string
  at: string
  kind: string
  actor?: string
  target?: string
  message: string
}
export type Retention = {
  jobs_days: number
  events_days: number
  metrics_raw_hours: number
  metrics_rollup_days: number
}
export type PurgeResult = { deleted: number }
export type TimelineAttempt = {
  task_id: string
  task_key: string
  attempt: number
  agent_id: string
  agent_name?: string
  started_at: string
  finished_at?: string
  outcome: string
  error?: string
  score: number
}
export type MacAttempt = TimelineAttempt & { job_id: string; job_name: string }

export const terminal = (state: string) =>
  ["succeeded", "failed", "cancelled"].includes(state)

// compact formats 1,284 / 12.9K / 4.2M.
export function compact(n: number | undefined): string {
  if (n === undefined || Number.isNaN(n)) return "–"
  return new Intl.NumberFormat(undefined, {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(n)
}

export function percent(ratio: number | undefined): string {
  if (ratio === undefined || ratio < 0) return "–"
  return `${(ratio * 100).toFixed(ratio === 1 || ratio === 0 ? 0 : 1)}%`
}
