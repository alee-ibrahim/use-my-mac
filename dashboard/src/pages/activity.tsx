import { useCallback, useState } from "react"

import { EventList, PageHeader } from "@/components/ops"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { api, type OpsEvent } from "@/lib/api"
import { usePolling } from "@/lib/hooks"

const kinds = [
  { value: "all", label: "Everything" },
  { value: "mac.", label: "Macs" },
  { value: "task.yielded", label: "Yields" },
  { value: "job.", label: "Jobs" },
  { value: "mac.update", label: "Agent updates" },
  { value: "key.,install_token.,jobs.purged,job.deleted,mac.deleted,retention.", label: "Admin changes" },
]

// ActivityPage: the activity and audit log (enrolments, yields, failed jobs,
// update results and every admin deletion, with who did it).
export function ActivityPage() {
  const [kind, setKind] = useState("all")
  const [older, setOlder] = useState<OpsEvent[]>([])
  const [more, setMore] = useState(true)
  const load = useCallback(() => api.events({ kind: kind === "all" ? "" : kind, limit: 50 }), [kind])
  const { data, error } = usePolling(load, 15000)

  async function loadMore() {
    const all = [...(data ?? []), ...older]
    const last = all[all.length - 1]
    if (!last) return
    const next = await api.events({ kind: kind === "all" ? "" : kind, limit: 50, before: last.at })
    setOlder((o) => [...o, ...next])
    setMore(next.length === 50)
  }

  const events = data && [...data, ...older.filter((o) => !data.some((d) => d.id === o.id))]
  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader title="Activity" description="Fleet events and the audit log of admin changes, newest first.">
        <Select
          value={kind}
          onValueChange={(v) => {
            setKind(v)
            setOlder([])
            setMore(true)
          }}
        >
          <SelectTrigger className="w-44 max-md:h-10" aria-label="Filter events">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {kinds.map((k) => (
              <SelectItem key={k.value} value={k.value}>
                {k.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </PageHeader>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <Card size="sm">
        <CardContent>
          <EventList events={events} />
          {events && events.length >= 50 && more && (
            <Button variant="outline" className="mt-3 w-full max-md:h-11" onClick={() => void loadMore()}>
              Load older
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
