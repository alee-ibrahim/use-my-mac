import { useEffect, useState } from "react"
import { toast } from "sonner"
import { PencilIcon } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { api, type APIKey, type Mac } from "@/lib/api"

// Router-side placement settings of a Mac: labels merged over the agent's,
// a reservation for some API keys, and how many tasks it runs at once.

function LabelBadges({
  labels,
  variant = "outline",
}: {
  labels?: Record<string, string>
  variant?: "outline" | "secondary"
}) {
  const entries = Object.entries(labels ?? {})
  if (entries.length === 0) return <span className="text-muted-foreground">none</span>
  return (
    <span className="flex flex-wrap gap-1">
      {entries.map(([k, v]) => (
        <Badge key={k} variant={variant}>
          {k}={v}
        </Badge>
      ))}
    </span>
  )
}

export function ReservationLine({ m }: { m: Mac }) {
  const r = m.reservation
  if (!r) return <span className="text-muted-foreground">Not reserved: runs anyone's tasks</span>
  const owners = (r.owners ?? []).map((o) => r.owner_names?.[o] ?? o)
  return (
    <span>
      Reserved for {owners.length ? owners.join(", ") : r.mine ? "you" : "other owners"}
      {r.until ? ` until ${new Date(r.until).toLocaleString()}` : ""}
      {r.note ? ` · ${r.note}` : ""}
      {r.by ? <span className="text-muted-foreground"> · by {r.by}</span> : null}
    </span>
  )
}

export function MacPlacementCard({
  m,
  admin,
  onChanged,
}: {
  m: Mac
  admin: boolean
  onChanged: () => void
}) {
  const [editing, setEditing] = useState(false)
  return (
    <Card size="sm">
      <CardHeader>
        <CardTitle>Placement</CardTitle>
        <CardDescription>Which tasks this Mac takes, and how many at once</CardDescription>
        {admin && (
          <CardAction>
            <Button variant="outline" size="sm" className="max-md:h-10" onClick={() => setEditing(true)}>
              <PencilIcon data-icon="inline-start" />
              Edit
            </Button>
          </CardAction>
        )}
      </CardHeader>
      <CardContent className="flex flex-col gap-2 text-sm">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-muted-foreground">Agent labels</span>
          <LabelBadges labels={m.agent_labels} />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-muted-foreground">Router labels</span>
          <LabelBadges labels={m.admin_labels} variant="secondary" />
        </div>
        <ReservationLine m={m} />
        <span>
          {m.max_tasks} task{m.max_tasks === 1 ? "" : "s"} at once
          <span className="text-muted-foreground">
            {m.max_tasks_set
              ? ` · set on the router to ${m.max_tasks_set}${m.agent_max_tasks ? `, the Mac allows up to ${m.agent_max_tasks}` : " (agent too old to apply it)"}`
              : " · from the Mac's agent.yaml"}
          </span>
        </span>
      </CardContent>
      {editing && (
        <EditPlacement m={m} onClose={() => setEditing(false)} onSaved={onChanged} />
      )}
    </Card>
  )
}

function labelsToText(l?: Record<string, string>) {
  return Object.entries(l ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join("\n")
}

function parseLabels(text: string): Record<string, string> | string {
  const out: Record<string, string> = {}
  for (const raw of text.split("\n")) {
    const line = raw.trim()
    if (!line) continue
    const i = line.indexOf("=")
    if (i <= 0) return `"${line}" is not key=value`
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  return out
}

function EditPlacement({ m, onClose, onSaved }: { m: Mac; onClose: () => void; onSaved: () => void }) {
  const [labels, setLabels] = useState(labelsToText(m.admin_labels))
  const [owners, setOwners] = useState<Set<string>>(new Set(m.reservation?.owners ?? []))
  const [hours, setHours] = useState("")
  const [note, setNote] = useState(m.reservation?.note ?? "")
  const [maxTasks, setMaxTasks] = useState(String(m.max_tasks_set ?? 0))
  const [keys, setKeys] = useState<APIKey[]>()
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    api
      .keys()
      .then((ks) => setKeys(ks.filter((k) => !k.revoked_at && k.role !== "viewer")))
      .catch(() => setKeys([]))
  }, [])
  const parsed = parseLabels(labels)
  const n = Number(maxTasks)
  const maxOK = Number.isInteger(n) && n >= 0 && n <= 64
  const hoursOK = hours === "" || (Number.isInteger(Number(hours)) && Number(hours) > 0)
  const valid = typeof parsed !== "string" && maxOK && hoursOK

  function toggle(o: string, on: boolean) {
    setOwners((s) => {
      const next = new Set(s)
      if (on) next.add(o)
      else next.delete(o)
      return next
    })
  }

  async function save() {
    if (typeof parsed === "string") return
    setBusy(true)
    try {
      if (labelsToText(parsed) !== labelsToText(m.admin_labels)) await api.setMacLabels(m.id, parsed)
      const before = [...(m.reservation?.owners ?? [])].sort().join(",")
      const after = [...owners].sort().join(",")
      if (owners.size === 0 && m.reservation) await api.unreserveMac(m.id)
      else if (owners.size > 0 && (after !== before || hours !== "" || note !== (m.reservation?.note ?? "")))
        await api.reserveMac(m.id, { owners: [...owners], hours: hours ? Number(hours) : undefined, note })
      if (n !== (m.max_tasks_set ?? 0)) await api.patchMac(m.id, { max_tasks: n })
      toast.success("Saved")
      onSaved()
      onClose()
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  // Owners already on the reservation that are not API keys (users) stay listed.
  const extra = [...owners].filter((o) => !keys?.some((k) => `key:${k.id}` === o))

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Placement for {m.name}</DialogTitle>
          <DialogDescription>
            Router labels are merged over the Mac's own for task matching. A reserved Mac runs only the chosen
            owners' tasks.
          </DialogDescription>
        </DialogHeader>
        <FieldGroup>
          <Field data-invalid={typeof parsed === "string"}>
            <FieldLabel htmlFor="mac-labels">Router labels</FieldLabel>
            <Textarea
              id="mac-labels"
              rows={3}
              placeholder={"class=studio\nroom=2"}
              value={labels}
              onChange={(e) => setLabels(e.target.value)}
            />
            <FieldDescription>
              {typeof parsed === "string" ? parsed : "One key=value per line. They win over the agent's labels."}
            </FieldDescription>
          </Field>
          <FieldSet>
            <FieldLegend variant="label">Reserved for</FieldLegend>
            <FieldDescription>No owner ticked: not reserved.</FieldDescription>
            {!keys ? (
              <span className="text-sm text-muted-foreground">Loading API keys…</span>
            ) : (
              <div className="flex flex-col gap-2">
                {keys.map((k) => (
                  <Field key={k.id} orientation="horizontal">
                    <Checkbox
                      id={`own-${k.id}`}
                      checked={owners.has(`key:${k.id}`)}
                      onCheckedChange={(v) => toggle(`key:${k.id}`, v === true)}
                    />
                    <FieldLabel htmlFor={`own-${k.id}`} className="font-normal">
                      {k.name} <span className="font-mono text-xs text-muted-foreground">{k.id}</span>
                    </FieldLabel>
                  </Field>
                ))}
                {extra.map((o) => (
                  <Field key={o} orientation="horizontal">
                    <Checkbox id={`own-${o}`} checked onCheckedChange={(v) => toggle(o, v === true)} />
                    <FieldLabel htmlFor={`own-${o}`} className="font-mono text-xs font-normal">
                      {o}
                    </FieldLabel>
                  </Field>
                ))}
              </div>
            )}
          </FieldSet>
          {owners.size > 0 && (
            <>
              <Field data-invalid={!hoursOK}>
                <FieldLabel htmlFor="res-hours">Ends after (hours)</FieldLabel>
                <Input
                  id="res-hours"
                  inputMode="numeric"
                  placeholder={m.reservation?.until ? "keep the current end" : "until removed"}
                  value={hours}
                  onChange={(e) => setHours(e.target.value)}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="res-note">Note</FieldLabel>
                <Input id="res-note" value={note} maxLength={200} onChange={(e) => setNote(e.target.value)} />
              </Field>
            </>
          )}
          <Field data-invalid={!maxOK}>
            <FieldLabel htmlFor="max-tasks">Tasks at once</FieldLabel>
            <Input
              id="max-tasks"
              inputMode="numeric"
              value={maxTasks}
              onChange={(e) => setMaxTasks(e.target.value)}
            />
            <FieldDescription>
              1-64, or 0 for the Mac's own setting. The Mac caps it at its max_tasks_limit
              {m.agent_max_tasks ? ` (${m.agent_max_tasks})` : ""}.
            </FieldDescription>
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={!valid || busy} onClick={save}>
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
