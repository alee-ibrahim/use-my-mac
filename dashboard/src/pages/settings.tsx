import { useEffect, useState } from "react"
import { toast } from "sonner"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { api, type Me, type Settings } from "@/lib/api"

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

export function SettingsPage({ me }: { me: Me }) {
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
    </div>
  )
}
