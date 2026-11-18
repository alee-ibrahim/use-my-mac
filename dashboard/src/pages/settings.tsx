import { useEffect, useState } from "react"
import { toast } from "sonner"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { api, type Mac, type Me, type Settings, type UpdatesInfo } from "@/lib/api"
import { AccessPanel } from "@/pages/access"

const weightHelp: Record<string, string> = {
  presence: "Prefer Macs with nobody logged in, a locked screen or an idle user.",
  load: "Prefer Macs with low total CPU load.",
  spread: "Prefer Macs running fewer of our tasks.",
  memory: "Prefer memory headroom after placing the task.",
  power: "Prefer Macs on AC power.",
  thermal: "Prefer Macs with a nominal thermal state.",
  gpu: "Prefer an idle GPU (GPU tasks only).",
  capacity: "Slightly prefer bigger Macs.",
}

// UpdatesCard edits the agent self-update policy: which version Macs run,
// an optional canary group that gets a new version first, and a pause switch.
function UpdatesCard({ admin }: { admin: boolean }) {
  const [info, setInfo] = useState<UpdatesInfo>()
  const [macs, setMacs] = useState<Mac[]>([])
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  useEffect(() => {
    api.updates().then(setInfo, (e) => setError(String(e)))
    api.macs().then(setMacs, () => setMacs([]))
  }, [])

  const s = info?.settings
  function set(patch: Partial<UpdatesInfo["settings"]>) {
    if (info) setInfo({ ...info, settings: { ...info.settings, ...patch } })
  }

  async function save(e: React.FormEvent) {
    e.preventDefault()
    if (!s) return
    setSaving(true)
    try {
      setInfo(await api.saveUpdates(s))
      toast.success("Update settings saved")
    } catch (err) {
      toast.error(String(err))
    } finally {
      setSaving(false)
    }
  }

  const canaryMacs = s?.canary_macs ?? []
  return (
    <Card>
      <form onSubmit={save}>
        <CardHeader>
          <CardTitle>Agent updates</CardTitle>
          <CardDescription>
            Each Mac's root updater checks in every 15 minutes, installs signed releases only, waits for running
            tasks, and rolls back if the new agent does not report in. Router version{" "}
            <strong>{info?.router_version ?? "…"}</strong>
            {info ? (info.relay ? " · releases relayed by the router" : " · Macs download releases directly") : ""}.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {!s ? (
            <Spinner />
          ) : (
            <FieldGroup>
              <Field orientation="horizontal">
                <Switch id="upd-paused" disabled={!admin} checked={s.paused} onCheckedChange={(v) => set({ paused: v })} />
                <FieldLabel htmlFor="upd-paused">Pause all agent updates</FieldLabel>
              </Field>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field>
                  <FieldLabel htmlFor="upd-target">Target version</FieldLabel>
                  <Input
                    id="upd-target"
                    placeholder={`router version (${info?.router_version ?? ""})`}
                    disabled={!admin}
                    value={s.target_version}
                    onChange={(e) => set({ target_version: e.target.value })}
                  />
                  <FieldDescription>
                    Empty: follow the router's version (never downgrades). A pinned version also allows downgrades.
                  </FieldDescription>
                </Field>
                <Field>
                  <FieldLabel htmlFor="upd-drain">Wait for running tasks (minutes)</FieldLabel>
                  <Input
                    id="upd-drain"
                    type="number"
                    min={0}
                    disabled={!admin}
                    value={Math.round((s.drain_timeout_seconds || 3600) / 60)}
                    onChange={(e) => set({ drain_timeout_seconds: Number(e.target.value) * 60 })}
                  />
                  <FieldDescription>Then the agent restarts anyway; its tasks are requeued, not failed.</FieldDescription>
                </Field>
              </div>
              <FieldSet>
                <FieldLegend>Canary</FieldLegend>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="upd-canary">Canary version</FieldLabel>
                    <Input
                      id="upd-canary"
                      placeholder="none"
                      disabled={!admin}
                      value={s.canary_version}
                      onChange={(e) => set({ canary_version: e.target.value })}
                    />
                    <FieldDescription>Goes to the canary Macs first. Promote it by making it the target.</FieldDescription>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="upd-pct">Canary share of the fleet (%)</FieldLabel>
                    <Input
                      id="upd-pct"
                      type="number"
                      min={0}
                      max={100}
                      disabled={!admin}
                      value={s.canary_percent}
                      onChange={(e) => set({ canary_percent: Number(e.target.value) })}
                    />
                    <FieldDescription>A stable selection: the same Macs every time.</FieldDescription>
                  </Field>
                </div>
                {macs.length > 0 && (
                  <div className="grid gap-2 sm:grid-cols-2">
                    {macs.map((m) => (
                      <Field key={m.id} orientation="horizontal">
                        <Switch
                          id={`canary-${m.id}`}
                          disabled={!admin}
                          checked={canaryMacs.includes(m.id)}
                          onCheckedChange={(v) =>
                            set({ canary_macs: v ? [...canaryMacs, m.id] : canaryMacs.filter((x) => x !== m.id) })
                          }
                        />
                        <FieldLabel htmlFor={`canary-${m.id}`}>
                          {m.name} <span className="font-mono text-xs text-muted-foreground">{m.version}</span>
                        </FieldLabel>
                      </Field>
                    ))}
                  </div>
                )}
              </FieldSet>
            </FieldGroup>
          )}
        </CardContent>
        {admin && (
          <CardFooter className="mt-4">
            <Button type="submit" disabled={saving || !s}>
              {saving && <Spinner data-icon="inline-start" />}
              Save
            </Button>
          </CardFooter>
        )}
      </form>
    </Card>
  )
}

// SettingsPage: general settings for everyone; admins also get Access
// (dashboard roles for single-sign-on users).
export function SettingsPage({ me }: { me: Me }) {
  const [tab, setTab] = useState(() => (sessionStorage.getItem("settings-tab") === "access" ? "access" : "general"))
  if (me.role !== "admin") return <GeneralSettings me={me} />
  return (
    <Tabs
      value={tab}
      onValueChange={(v) => {
        setTab(v)
        sessionStorage.setItem("settings-tab", v)
      }}
    >
      <TabsList>
        <TabsTrigger value="general">General</TabsTrigger>
        <TabsTrigger value="access">Access</TabsTrigger>
      </TabsList>
      <TabsContent value="general">
        <GeneralSettings me={me} />
      </TabsContent>
      <TabsContent value="access">
        <AccessPanel me={me} />
      </TabsContent>
    </Tabs>
  )
}

function GeneralSettings({ me }: { me: Me }) {
  const admin = me.role === "admin"
  const [s, setS] = useState<Settings>()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  useEffect(() => {
    api.settings().then(setS, (e) => setError(String(e)))
  }, [])

  async function save(e: React.FormEvent) {
    e.preventDefault()
    if (!s) return
    setSaving(true)
    try {
      setS(await api.saveSettings(s))
      toast.success("Scheduler settings saved")
    } catch (err) {
      toast.error(String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Signed in</CardTitle>
          <CardDescription>
            {me.name} · role <strong>{me.role}</strong> · via {me.via}
          </CardDescription>
        </CardHeader>
      </Card>
      <Card>
        <form onSubmit={save}>
          <CardHeader>
            <CardTitle>Scheduler</CardTitle>
            <CardDescription>
              Weights are relative. Hard limits always apply. Each Mac's own yield thresholds live in its agent
              config so the Mac's owner stays in control.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {error && (
              <Alert variant="destructive">
                <AlertDescription>{error}</AlertDescription>
              </Alert>
            )}
            {!s ? (
              <Spinner />
            ) : (
              <FieldGroup>
                <FieldSet>
                  <FieldLegend>Score weights</FieldLegend>
                  <div className="grid gap-4 sm:grid-cols-2">
                    {Object.keys(weightHelp).map((k) => (
                      <Field key={k}>
                        <FieldLabel htmlFor={`w-${k}`} className="capitalize">
                          {k}
                        </FieldLabel>
                        <Input
                          id={`w-${k}`}
                          type="number"
                          min={0}
                          max={100}
                          step={0.1}
                          disabled={!admin}
                          value={s.weights[k] ?? 0}
                          onChange={(e) => setS({ ...s, weights: { ...s.weights, [k]: Number(e.target.value) } })}
                        />
                        <FieldDescription>{weightHelp[k]}</FieldDescription>
                      </Field>
                    ))}
                  </div>
                </FieldSet>
                <FieldSet>
                  <FieldLegend>Limits</FieldLegend>
                  <div className="grid gap-4 sm:grid-cols-2">
                    <Field>
                      <FieldLabel htmlFor="idle">Idle counts as away after (seconds)</FieldLabel>
                      <Input
                        id="idle"
                        type="number"
                        min={1}
                        disabled={!admin}
                        value={s.limits.idle_full_seconds}
                        onChange={(e) => setS({ ...s, limits: { ...s.limits, idle_full_seconds: Number(e.target.value) } })}
                      />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="disk">Keep free disk (MB)</FieldLabel>
                      <Input
                        id="disk"
                        type="number"
                        min={0}
                        disabled={!admin}
                        value={s.limits.min_disk_free_mb}
                        onChange={(e) => setS({ ...s, limits: { ...s.limits, min_disk_free_mb: Number(e.target.value) } })}
                      />
                    </Field>
                    <Field orientation="horizontal">
                      <Switch
                        id="battery"
                        disabled={!admin}
                        checked={s.limits.allow_battery}
                        onCheckedChange={(v) => setS({ ...s, limits: { ...s.limits, allow_battery: v } })}
                      />
                      <FieldLabel htmlFor="battery">Allow work on battery power</FieldLabel>
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="minbat">Minimum battery (%)</FieldLabel>
                      <Input
                        id="minbat"
                        type="number"
                        min={0}
                        max={100}
                        disabled={!admin || !s.limits.allow_battery}
                        value={s.limits.min_battery_percent}
                        onChange={(e) => setS({ ...s, limits: { ...s.limits, min_battery_percent: Number(e.target.value) } })}
                      />
                    </Field>
                  </div>
                </FieldSet>
              </FieldGroup>
            )}
          </CardContent>
          {admin && (
            <CardFooter className="mt-4">
              <Button type="submit" disabled={saving || !s}>
                {saving && <Spinner data-icon="inline-start" />}
                Save
              </Button>
            </CardFooter>
          )}
        </form>
      </Card>
      <UpdatesCard admin={admin} />
    </div>
  )
}
