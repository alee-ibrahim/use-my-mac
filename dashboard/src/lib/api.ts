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
  labels: Record<string, string>
  max_tasks: number
  running: string[]
  telemetry?: Telemetry
  version?: string
  update?: MacUpdate
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
  resources?: { ram_mb?: number; gpu?: boolean; expected_seconds?: number }
  outputs?: { name: string }[]
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
  placement?: { id: string; ok: boolean; reason?: string; score: number; parts?: Record<string, number> }[]
}

export type Job = {
  id: string
  name: string
  owner: string
  state: string
  created_at: string
  finished_at?: string
  counts: Record<string, number>
  tasks?: Task[]
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

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
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
  return (ct.includes("application/json") ? res.json() : res.text()) as Promise<T>
}

// How the dashboard may be signed in to. apikey = API-key sign-in allowed.
export type AuthConfig = { oidc: boolean; apikey: boolean; auto_sign_in: boolean }

// Starts single sign-on and returns to `ret` (sanitised again by the server).
export function ssoURL(ret = "/") {
  return `/auth/login?return=${encodeURIComponent(ret)}`
}

export const api = {
  authConfig: () => request<AuthConfig>("GET", "/auth/config"),
  loginWithKey: (key: string) => request<Me>("POST", "/auth/apikey", { key }),
  logout: () => request<void>("POST", "/auth/logout"),
  me: () => request<Me>("GET", "/v1/me"),
  macs: () => request<Mac[]>("GET", "/v1/macs"),
  patchMac: (id: string, patch: { drain?: boolean; opted_out?: boolean }) =>
    request<void>("PATCH", `/v1/macs/${id}`, patch),
  deleteMac: (id: string) => request<void>("DELETE", `/v1/macs/${id}`),
  enrolToken: (ttl_hours: number) =>
    request<{ token: string; expires_at: string }>("POST", "/v1/enrolment-tokens", { ttl_hours }),
  jobs: () => request<Job[]>("GET", "/v1/jobs?limit=200"),
  job: (id: string) => request<Job>("GET", `/v1/jobs/${id}`),
  cancelJob: (id: string) => request<Job>("POST", `/v1/jobs/${id}/cancel`),
  task: (id: string) => request<Task>("GET", `/v1/tasks/${id}`),
  logs: (id: string) => request<string>("GET", `/v1/tasks/${id}/logs`),
  keys: () => request<APIKey[]>("GET", "/v1/api-keys"),
  createKey: (name: string, role: string) => request<APIKey>("POST", "/v1/api-keys", { name, role }),
  revokeKey: (id: string) => request<void>("DELETE", `/v1/api-keys/${id}`),
  settings: () => request<Settings>("GET", "/v1/settings"),
  saveSettings: (s: Settings) => request<Settings>("PUT", "/v1/settings", s),
  updates: () => request<UpdatesInfo>("GET", "/v1/updates"),
  saveUpdates: (s: UpdateSettings) => request<UpdatesInfo>("PUT", "/v1/updates", s),
  macUpdates: (id: string) => request<UpdateEvent[]>("GET", `/v1/macs/${id}/updates`),
  installProfiles: () => request<InstallProfile[]>("GET", "/v1/install/profiles"),
  installTokens: () => request<InstallToken[]>("GET", "/v1/install-tokens"),
  createInstallToken: (r: InstallTokenRequest) => request<InstallToken>("POST", "/v1/install-tokens", r),
  revokeInstallToken: (id: string) => request<void>("DELETE", `/v1/install-tokens/${id}`),
  installEvents: (id: string) => request<InstallEvent[]>("GET", `/v1/install-tokens/${id}/events`),
  access: () => request<AccessPolicy>("GET", "/v1/access"),
  putAccessRule: (r: AccessRuleRequest) => request<AccessRule>("POST", "/v1/access/rules", r),
  deleteAccessRule: (id: string) => request<void>("DELETE", `/v1/access/rules/${encodeURIComponent(id)}`),
  setAccessDefault: (role: string) => request<AccessPolicy>("PUT", "/v1/access/default", { role }),
  setGroupName: (id: string, name: string) =>
    request<void>("PUT", `/v1/access/groups/${encodeURIComponent(id)}`, { name }),
  accessUsers: () => request<AccessUser[]>("GET", "/v1/access/users"),
  setUserRole: (subject: string, role: string) =>
    request<void>("PUT", `/v1/access/users/${encodeURIComponent(subject)}/role`, { role }),
  accessAudit: () => request<AccessEvent[]>("GET", "/v1/access/audit?limit=200"),
  docs: () => request<DocInfo[]>("GET", "/v1/docs"),
  doc: (slug: string) => request<string>("GET", `/v1/docs/${encodeURIComponent(slug)}`),
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
export type InstallEvent = { at: string; kind: string; ip?: string; agent_id?: string; detail?: string }

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
export type AccessRuleRequest = { kind: string; value: string; role: string; name?: string }
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
export type AccessEvent = { at: string; actor: string; action: string; detail: string }
