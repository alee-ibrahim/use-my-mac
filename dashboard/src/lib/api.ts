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

// An embedded documentation page (public, served at /docs).
export type DocInfo = { slug: string; title: string; path: string }
