import { useCallback, useState } from "react"
import { ListChecksIcon, Trash2Icon } from "lucide-react"

import { useConfirm } from "@/components/confirm"
import { PageHeader } from "@/components/ops"
import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Progress } from "@/components/ui/progress"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useIsMobile } from "@/hooks/use-mobile"
import { api, since, terminal, type Job } from "@/lib/api"
import { href, usePolling } from "@/lib/hooks"

export function progress(j: Job) {
  const total = Object.values(j.counts).reduce((a, b) => a + b, 0)
  const done = (j.counts.succeeded ?? 0) + (j.counts.failed ?? 0) + (j.counts.cancelled ?? 0)
  return { total, done, pct: total ? (100 * done) / total : 0 }
}

const filters = [
  { value: "", label: "All" },
  { value: "running", label: "Running" },
  { value: "queued", label: "Queued" },
  { value: "succeeded", label: "Succeeded" },
  { value: "failed", label: "Failed" },
  { value: "cancelled", label: "Cancelled" },
]

// PurgeDialog deletes finished jobs older than N days (bulk cleanup).
export function PurgeJobsDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = useState(false)
  const [days, setDays] = useState("30")
  const [state, setState] = useState("all")
  const [confirm, ask] = useConfirm()
  const what = state === "all" ? "finished" : state
  return (
    <>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogTrigger asChild>
          <Button type="button" variant="outline" size="sm" className="max-md:h-10">
            <Trash2Icon data-icon="inline-start" />
            Delete old jobs…
          </Button>
        </DialogTrigger>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete old jobs</DialogTitle>
            <DialogDescription>
              Deletes finished jobs with their tasks, logs and the outputs kept on the router. Running and queued jobs
              are never touched. Settings → Data can do this automatically.
            </DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="purge-days">Finished more than</FieldLabel>
              <Select value={days} onValueChange={setDays}>
                <SelectTrigger id="purge-days">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {["1", "7", "14", "30", "90", "180", "365"].map((d) => (
                    <SelectItem key={d} value={d}>
                      {d} day{d === "1" ? "" : "s"} ago
                    </SelectItem>
                  ))}
                  <SelectItem value="0">any time (all finished jobs)</SelectItem>
                </SelectContent>
              </Select>
            </Field>
            <Field>
              <FieldLabel htmlFor="purge-state">Which</FieldLabel>
              <Select value={state} onValueChange={setState}>
                <SelectTrigger id="purge-state">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">All finished jobs</SelectItem>
                  <SelectItem value="succeeded">Succeeded only</SelectItem>
                  <SelectItem value="failed">Failed only</SelectItem>
                  <SelectItem value="cancelled">Cancelled only</SelectItem>
                </SelectContent>
              </Select>
            </Field>
          </FieldGroup>
          <DialogFooter>
            <Button
              variant="destructive"
              onClick={() =>
                ask({
                  title: `Delete ${what} jobs?`,
                  description: `Every ${what} job that finished ${days === "0" ? "at any time" : `more than ${days} days ago`} is deleted for good, with its logs and stored outputs.`,
                  action: "Delete jobs",
                  run: async () => {
                    const r = await api.purgeJobs(+days, state === "all" ? undefined : [state])
                    setOpen(false)
                    onDone()
                    return `Deleted ${r.deleted} job${r.deleted === 1 ? "" : "s"}`
                  },
                })
              }
            >
              Delete…
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      {confirm}
    </>
  )
}

export function JobsPage({ admin }: { admin: boolean }) {
  const [state, setState] = useState("")
  const load = useCallback(() => api.jobs(state, 200), [state])
  const { data, error, refresh } = usePolling(load, 5000)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [confirm, ask] = useConfirm()
  const mobile = useIsMobile()

  const jobs = data ?? []
  const deletable = jobs.filter((j) => terminal(j.state))
  const sel = deletable.filter((j) => selected.has(j.id))
  const toggle = (id: string, on: boolean) =>
    setSelected((s) => {
      const n = new Set(s)
      if (on) n.add(id)
      else n.delete(id)
      return n
    })

  function deleteSelected() {
    ask({
      title: `Delete ${sel.length} job${sel.length === 1 ? "" : "s"}?`,
      description: "Their tasks, logs and stored outputs are deleted for good.",
      action: "Delete",
      run: async () => {
        let n = 0
        for (const j of sel) {
          await api.deleteJob(j.id)
          n++
        }
        setSelected(new Set())
        void refresh()
        return `Deleted ${n} job${n === 1 ? "" : "s"}`
      },
    })
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader title="Jobs" description="Most recent first. Open a job for its timeline, tasks, retries and logs.">
        {admin && sel.length > 0 && (
          <Button variant="destructive" size="sm" className="max-md:h-10" onClick={deleteSelected}>
            <Trash2Icon data-icon="inline-start" />
            Delete {sel.length}
          </Button>
        )}
        {admin && <PurgeJobsDialog onDone={() => void refresh()} />}
      </PageHeader>
      <Tabs value={state} onValueChange={(v) => (setState(v), setSelected(new Set()))}>
        <div className="-mx-1 overflow-x-auto px-1">
          <TabsList>
            {filters.map((f) => (
              <TabsTrigger key={f.value} value={f.value} className="max-md:h-9">
                {f.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>
      </Tabs>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {!data ? (
        <Skeleton className="h-64 w-full rounded-xl" />
      ) : jobs.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <ListChecksIcon />
            </EmptyMedia>
            <EmptyTitle>No jobs{state ? ` ${state}` : " yet"}</EmptyTitle>
            <EmptyDescription>Submit one with the umm CLI or the REST API.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : mobile ? (
        <ul className="flex flex-col gap-2">
          {jobs.map((j) => {
            const p = progress(j)
            return (
              <li key={j.id}>
                <Card size="sm">
                  <CardContent className="flex items-center gap-3">
                    {admin && (
                      <Checkbox
                        className="size-5"
                        aria-label={`Select ${j.name}`}
                        disabled={!terminal(j.state)}
                        checked={selected.has(j.id)}
                        onCheckedChange={(c) => toggle(j.id, c === true)}
                      />
                    )}
                    <a href={href("job", j.id)} className="flex min-h-11 min-w-0 flex-1 flex-col gap-1.5">
                      <div className="flex items-center justify-between gap-2">
                        <span className="truncate font-medium">{j.name || j.id}</span>
                        <StateBadge state={j.state} />
                      </div>
                      <div className="flex items-center gap-2">
                        <Progress value={p.pct} aria-label="job progress" />
                        <span className="shrink-0 text-xs tabular-nums">
                          {p.done}/{p.total}
                        </span>
                      </div>
                      <span className="text-xs text-muted-foreground">
                        {since(j.created_at)} · {j.owner}
                      </span>
                    </a>
                  </CardContent>
                </Card>
              </li>
            )
          })}
        </ul>
      ) : (
        <Card size="sm">
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  {admin && (
                    <TableHead className="w-8">
                      <Checkbox
                        aria-label="Select all finished jobs"
                        checked={sel.length > 0 && sel.length === deletable.length ? true : sel.length > 0 ? "indeterminate" : false}
                        disabled={deletable.length === 0}
                        onCheckedChange={(c) => setSelected(c === true ? new Set(deletable.map((j) => j.id)) : new Set())}
                      />
                    </TableHead>
                  )}
                  <TableHead>Job</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Progress</TableHead>
                  <TableHead className="hidden lg:table-cell">Owner</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="hidden lg:table-cell">Finished</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {jobs.map((j) => {
                  const p = progress(j)
                  return (
                    <TableRow
                      key={j.id}
                      className="cursor-pointer"
                      data-state={selected.has(j.id) ? "selected" : undefined}
                      onClick={() => (location.hash = href("job", j.id))}
                    >
                      {admin && (
                        <TableCell onClick={(e) => e.stopPropagation()}>
                          <Checkbox
                            aria-label={`Select ${j.name}`}
                            disabled={!terminal(j.state)}
                            checked={selected.has(j.id)}
                            onCheckedChange={(c) => toggle(j.id, c === true)}
                          />
                        </TableCell>
                      )}
                      <TableCell>
                        <a
                          href={href("job", j.id)}
                          className="font-medium hover:underline"
                          onClick={(e) => e.stopPropagation()}
                        >
                          {j.name || j.id}
                        </a>
                        <div className="font-mono text-xs text-muted-foreground">{j.id}</div>
                      </TableCell>
                      <TableCell>
                        <StateBadge state={j.state} />
                      </TableCell>
                      <TableCell>
                        <div className="flex min-w-36 items-center gap-2">
                          <Progress value={p.pct} aria-label="job progress" />
                          <span className="text-xs tabular-nums">
                            {p.done}/{p.total}
                          </span>
                        </div>
                      </TableCell>
                      <TableCell className="hidden max-w-48 truncate text-muted-foreground lg:table-cell">
                        {j.owner}
                      </TableCell>
                      <TableCell className="text-muted-foreground">{since(j.created_at)}</TableCell>
                      <TableCell className="hidden text-muted-foreground lg:table-cell">
                        {since(j.finished_at)}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
      {confirm}
    </div>
  )
}
