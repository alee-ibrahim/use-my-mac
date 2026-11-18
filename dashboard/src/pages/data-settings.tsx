import { useEffect, useState } from "react"
import { toast } from "sonner"
import { Trash2Icon } from "lucide-react"

import { useConfirm } from "@/components/confirm"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Separator } from "@/components/ui/separator"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { api, type Retention } from "@/lib/api"
import { PurgeJobsDialog } from "@/pages/jobs"

// DataCard: how long the router keeps history, plus one-off cleanup.
// Viewers see the values read-only.
export function DataCard({ admin }: { admin: boolean }) {
  const [r, setR] = useState<Retention>()
  const [error, setError] = useState<string>()
  const [saving, setSaving] = useState(false)
  const [confirm, ask] = useConfirm()

  useEffect(() => {
    api.retention().then(setR, (e) => setError(String(e)))
  }, [])

  async function save(e: React.FormEvent) {
    e.preventDefault()
    if (!r) return
    setSaving(true)
    try {
      setR(await api.saveRetention(r))
      toast.success("Retention saved")
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  const num = (k: keyof Retention) => (e: React.ChangeEvent<HTMLInputElement>) =>
    r && setR({ ...r, [k]: Number(e.target.value) })

  return (
    <Card>
      <form onSubmit={save}>
        <CardHeader>
          <CardTitle>Data and retention</CardTitle>
          <CardDescription>
            Metrics are sampled every minute and kept at 1-minute resolution for a day, then as 15-minute averages.
            Pruning runs hourly.
          </CardDescription>
        </CardHeader>
        <CardContent className="mt-2">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {!r ? (
            <Spinner />
          ) : (
            <FieldGroup>
              <FieldSet>
                <FieldLegend>Finished jobs</FieldLegend>
                <Field orientation="horizontal">
                  <Switch
                    id="ret-jobs-auto"
                    disabled={!admin}
                    checked={r.jobs_days > 0}
                    onCheckedChange={(v) => setR({ ...r, jobs_days: v ? 30 : 0 })}
                  />
                  <FieldLabel htmlFor="ret-jobs-auto">Delete finished jobs automatically</FieldLabel>
                </Field>
                {r.jobs_days > 0 && (
                  <Field>
                    <FieldLabel htmlFor="ret-jobs">After (days)</FieldLabel>
                    <Input
                      id="ret-jobs"
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={3650}
                      disabled={!admin}
                      value={r.jobs_days}
                      onChange={num("jobs_days")}
                    />
                    <FieldDescription>With their tasks, logs and the outputs stored on the router.</FieldDescription>
                  </Field>
                )}
              </FieldSet>
              <FieldSet>
                <FieldLegend>History</FieldLegend>
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field>
                    <FieldLabel htmlFor="ret-raw">1-minute metrics (hours)</FieldLabel>
                    <Input
                      id="ret-raw"
                      type="number"
                      inputMode="numeric"
                      min={24}
                      max={168}
                      disabled={!admin}
                      value={r.metrics_raw_hours}
                      onChange={num("metrics_raw_hours")}
                    />
                    <FieldDescription>24 to 168.</FieldDescription>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="ret-rollup">15-minute metrics (days)</FieldLabel>
                    <Input
                      id="ret-rollup"
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={400}
                      disabled={!admin}
                      value={r.metrics_rollup_days}
                      onChange={num("metrics_rollup_days")}
                    />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="ret-events">Activity log (days)</FieldLabel>
                    <Input
                      id="ret-events"
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={3650}
                      disabled={!admin}
                      value={r.events_days}
                      onChange={num("events_days")}
                    />
                  </Field>
                </div>
              </FieldSet>
            </FieldGroup>
          )}
        </CardContent>
        {admin && (
          <CardFooter className="mt-4 flex flex-col items-stretch gap-4">
            <div>
              <Button type="submit" disabled={saving || !r}>
                {saving && <Spinner data-icon="inline-start" />}
                Save retention
              </Button>
            </div>
            <Separator />
            <div className="flex flex-col gap-2">
              <span className="text-sm font-medium">Clean up now</span>
              <div className="flex flex-wrap gap-2">
                <PurgeJobsDialog onDone={() => undefined} />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="max-md:h-10"
                  onClick={() =>
                    ask({
                      title: "Delete all revoked API keys?",
                      description: "Active keys are kept. Each deletion is recorded in the activity log.",
                      run: async () => `Deleted ${(await api.purgeKeys()).deleted} revoked keys`,
                    })
                  }
                >
                  <Trash2Icon data-icon="inline-start" />
                  Revoked keys…
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="max-md:h-10"
                  onClick={() =>
                    ask({
                      title: "Delete used, expired and revoked install links?",
                      description: "Links that still work are kept, and installed Macs are not affected.",
                      run: async () => `Deleted ${(await api.purgeInstallTokens()).deleted} install links`,
                    })
                  }
                >
                  <Trash2Icon data-icon="inline-start" />
                  Old install links…
                </Button>
              </div>
            </div>
          </CardFooter>
        )}
      </form>
      {confirm}
    </Card>
  )
}
