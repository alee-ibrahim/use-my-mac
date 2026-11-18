import { useCallback, useEffect, useMemo, useState } from "react"
import { toast } from "sonner"
import { ArrowLeftIcon, Trash2Icon, XCircleIcon } from "lucide-react"
import { Bar, BarChart, CartesianGrid, Cell, XAxis, YAxis } from "recharts"

import { useConfirm } from "@/components/confirm"
import { PageHeader, StatTile } from "@/components/ops"
import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart"
import {
  Drawer,
  DrawerContent,
  DrawerDescription,
  DrawerHeader,
  DrawerTitle,
} from "@/components/ui/drawer"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { useIsMobile } from "@/hooks/use-mobile"
import { api, duration, since, terminal, type Task, type TimelineAttempt } from "@/lib/api"
import { href, useNow, usePolling } from "@/lib/hooks"
import { progress } from "@/pages/jobs"

// Outcome groups for the timeline, in categorical slot order.
const outcomeGroups = [
  { key: "succeeded", label: "Succeeded", color: "var(--chart-1)", match: ["succeeded"] },
  { key: "failed", label: "Failed / timeout", color: "var(--chart-2)", match: ["failed", "timeout"] },
  { key: "running", label: "Running", color: "var(--chart-3)", match: ["running"] },
  { key: "yielded", label: "Yielded", color: "var(--chart-4)", match: ["yielded"] },
  {
    key: "other",
    label: "Lost / cancelled / Mac fault",
    color: "var(--chart-muted)",
    match: ["lost", "cancelled", "unavailable"],
  },
]
const groupOf = (outcome: string) => outcomeGroups.find((g) => g.match.includes(outcome)) ?? outcomeGroups[4]
const timelineConfig = Object.fromEntries(outcomeGroups.map((g) => [g.key, { label: g.label, color: g.color }])) as ChartConfig

const MAX_ROWS = 300

function Timeline({ attempts, by, now }: { attempts: TimelineAttempt[]; by: "task" | "mac"; now: number }) {
  const rows = useMemo(() => {
    const list = attempts.map((a) => {
      const start = new Date(a.started_at).getTime()
      const end = a.finished_at ? new Date(a.finished_at).getTime() : now
      const mac = a.agent_name || a.agent_id
      return {
        label: by === "mac" ? `${mac} · ${a.task_key}` : `${a.task_key} #${a.attempt}`,
        range: [start, Math.max(end, start + 1000)] as [number, number],
        a,
        mac,
        group: groupOf(a.outcome),
      }
    })
    if (by === "mac") list.sort((x, y) => x.mac.localeCompare(y.mac) || x.range[0] - y.range[0])
    return list.slice(0, MAX_ROWS)
  }, [attempts, by, now])
  if (rows.length === 0) return <p className="text-sm text-muted-foreground">No attempts yet.</p>
  const min = Math.min(...rows.map((r) => r.range[0]))
  const max = Math.max(...rows.map((r) => r.range[1]))
  const span = max - min
  const fmt = (t: number) =>
    span < 36 * 3600_000
      ? new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: span < 600_000 ? "2-digit" : undefined })
      : new Date(t).toLocaleDateString([], { month: "short", day: "numeric", hour: "2-digit" })
  return (
    <div className="flex flex-col gap-2">
      <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs" aria-label="Legend">
        {outcomeGroups.map((g) => (
          <li key={g.key} className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-[2px]" style={{ background: g.color }} aria-hidden />
            {g.label}
          </li>
        ))}
      </ul>
      <div className="max-h-[32rem] overflow-y-auto">
        <ChartContainer config={timelineConfig} className="aspect-auto w-full" style={{ height: rows.length * 24 + 40 }}>
          <BarChart data={rows} layout="vertical" margin={{ left: 0, right: 12, top: 4, bottom: 0 }} barCategoryGap={4}>
            <CartesianGrid horizontal={false} />
            <XAxis
              type="number"
              domain={[min, max]}
              scale="time"
              tickFormatter={fmt}
              tickLine={false}
              axisLine={false}
              minTickGap={48}
            />
            <YAxis
              type="category"
              dataKey="label"
              width={140}
              tickLine={false}
              axisLine={false}
              interval={0}
              tick={{ fontSize: 11 }}
            />
            <ChartTooltip
              cursor={{ fillOpacity: 0.3 }}
              content={
                <ChartTooltipContent
                  hideIndicator
                  labelFormatter={(_, p) => p?.[0]?.payload?.label ?? ""}
                  formatter={(_, __, item) => {
                    const r = item.payload as (typeof rows)[number]
                    return (
                      <div className="flex flex-col gap-0.5">
                        <span>
                          {r.group.label} on {r.mac}
                        </span>
                        <span className="text-muted-foreground">
                          {new Date(r.range[0]).toLocaleTimeString()} · {duration((r.range[1] - r.range[0]) / 1000)}
                        </span>
                        {r.a.error && <span className="max-w-64 text-muted-foreground">{r.a.error}</span>}
                      </div>
                    )
                  }}
                />
              }
            />
            <Bar dataKey="range" radius={4} maxBarSize={16} isAnimationActive={false}>
              {rows.map((r, i) => (
                <Cell key={i} fill={r.group.color} />
              ))}
            </Bar>
          </BarChart>
        </ChartContainer>
      </div>
      {attempts.length > MAX_ROWS && (
        <p className="text-xs text-muted-foreground">Showing the first {MAX_ROWS} of {attempts.length} attempts.</p>
      )}
    </div>
  )
}

function TaskBody({ task, logs }: { task?: Task; logs?: string }) {
  if (!task) {
    return (
      <div className="flex justify-center p-6">
        <Spinner />
      </div>
    )
  }
  return (
    <Tabs defaultValue="attempts" className="px-4 pb-4">
      <TabsList>
        <TabsTrigger value="attempts">Attempts</TabsTrigger>
        <TabsTrigger value="logs">Logs</TabsTrigger>
        <TabsTrigger value="placement">Placement</TabsTrigger>
      </TabsList>
      <TabsContent value="attempts">
        <ul className="flex flex-col">
          {(task.attempts ?? []).map((a) => (
            <li key={a.attempt} className="flex flex-col gap-1 border-b py-2 text-sm last:border-b-0">
              <div className="flex flex-wrap items-center gap-2">
                <span className="tabular-nums">#{a.attempt}</span>
                <StateBadge state={a.outcome} />
                <a href={href("mac", a.agent_id)} className="hover:underline">
                  {a.agent_name || a.agent_id}
                </a>
                <span className="text-xs text-muted-foreground">score {a.score.toFixed(1)}</span>
              </div>
              {a.error && <span className="text-xs text-muted-foreground">{a.error}</span>}
            </li>
          ))}
        </ul>
        {task.error && (
          <Alert className="mt-4">
            <AlertDescription>{task.error}</AlertDescription>
          </Alert>
        )}
      </TabsContent>
      <TabsContent value="logs">
        <ScrollArea className="h-[55vh] rounded-md border">
          <pre className="p-3 font-mono text-xs whitespace-pre-wrap">{logs || "No output yet."}</pre>
        </ScrollArea>
      </TabsContent>
      <TabsContent value="placement">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Mac</TableHead>
              <TableHead>Score</TableHead>
              <TableHead>Why</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(task.placement ?? []).map((p) => (
              <TableRow key={p.id}>
                <TableCell className="font-mono text-xs">{p.id}</TableCell>
                <TableCell className="tabular-nums">{p.ok ? p.score.toFixed(1) : "–"}</TableCell>
                <TableCell className="whitespace-normal text-muted-foreground">
                  {p.ok
                    ? Object.entries(p.parts ?? {})
                        .map(([k, v]) => `${k} ${v.toFixed(2)}`)
                        .join(" · ")
                    : p.reason}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TabsContent>
    </Tabs>
  )
}

// TaskPanel: a side sheet on desktop, a bottom drawer on phones.
function TaskPanel({ id, onClose }: { id?: string; onClose: () => void }) {
  const [task, setTask] = useState<Task>()
  const [logs, setLogs] = useState<string>()
  const mobile = useIsMobile()
  useEffect(() => {
    if (!id) return
    api.task(id).then(setTask, (e) => toast.error(String(e)))
    api.logs(id).then(setLogs, () => setLogs(""))
  }, [id])
  const title = `Task ${task?.key ?? ""}`
  const desc = task ? (
    <>
      <code>{task.spec.runtime}</code> {task.spec.args?.join(" ")}
    </>
  ) : (
    "Loading…"
  )
  if (mobile) {
    return (
      <Drawer open={!!id} onOpenChange={(o) => !o && onClose()}>
        <DrawerContent className="max-h-[90svh]">
          <DrawerHeader>
            <DrawerTitle>{title}</DrawerTitle>
            <DrawerDescription className="break-all">{desc}</DrawerDescription>
          </DrawerHeader>
          <div className="overflow-y-auto">
            <TaskBody task={task} logs={logs} />
          </div>
        </DrawerContent>
      </Drawer>
    )
  }
  return (
    <Sheet open={!!id} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-full sm:max-w-2xl">
        <SheetHeader>
          <SheetTitle>{title}</SheetTitle>
          <SheetDescription>{desc}</SheetDescription>
        </SheetHeader>
        <TaskBody task={task} logs={logs} />
      </SheetContent>
    </Sheet>
  )
}

export function JobDetailPage({ id, admin }: { id: string; admin: boolean }) {
  const { data: job, error, refresh } = usePolling(
    useCallback(() => api.job(id), [id]),
    3000
  )
  const tl = usePolling(
    useCallback(() => api.jobTimeline(id), [id]),
    5000
  )
  const [taskId, setTaskId] = useState<string>()
  const [by, setBy] = useState<"task" | "mac">("task")
  const [confirm, ask] = useConfirm()
  const mobile = useIsMobile()
  const now = useNow(5000)

  const macNames = useMemo(
    () => Object.fromEntries((tl.data ?? []).map((a) => [a.agent_id, a.agent_name || a.agent_id])),
    [tl.data]
  )
  const stats = useMemo(() => {
    const as = tl.data ?? []
    const macs = new Set(as.map((a) => a.agent_id))
    const count = (o: string[]) => as.filter((a) => o.includes(a.outcome)).length
    const start = job ? new Date(job.created_at).getTime() : 0
    const end = job?.finished_at ? new Date(job.finished_at).getTime() : now
    return {
      attempts: as.length,
      macs: macs.size,
      yields: count(["yielded"]),
      failures: count(["failed", "timeout"]),
      lost: count(["lost"]),
      elapsed: start ? (end - start) / 1000 : 0,
    }
  }, [tl.data, job, now])

  const back = (
    <Button variant="ghost" size="sm" className="self-start max-md:h-10" asChild>
      <a href={href("jobs")}>
        <ArrowLeftIcon data-icon="inline-start" />
        Jobs
      </a>
    </Button>
  )
  if (error && !job) {
    return (
      <div className="flex flex-col gap-4">
        {back}
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      </div>
    )
  }
  const p = job ? progress(job) : undefined

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      {back}
      <PageHeader
        title={
          job ? (
            <span className="flex items-center gap-2">
              {job.name || job.id} <StateBadge state={job.state} />
            </span>
          ) : (
            <Skeleton className="h-7 w-48" />
          )
        }
        description={
          job && (
            <span className="font-mono text-xs">
              {job.id} · {job.owner}
            </span>
          )
        }
      >
        {admin && job && !terminal(job.state) && (
          <Button
            variant="destructive"
            size="sm"
            className="max-md:h-10"
            onClick={() =>
              ask({
                title: "Cancel this job?",
                description: "Running tasks are stopped and nothing new starts.",
                action: "Cancel job",
                run: async () => {
                  await api.cancelJob(id)
                  void refresh()
                  return "Job cancelled"
                },
              })
            }
          >
            <XCircleIcon data-icon="inline-start" />
            Cancel job
          </Button>
        )}
        {admin && job && terminal(job.state) && (
          <Button
            variant="destructive"
            size="sm"
            className="max-md:h-10"
            onClick={() =>
              ask({
                title: "Delete this job?",
                description: "The job, its tasks, logs and stored outputs are deleted for good.",
                action: "Delete job",
                run: async () => {
                  await api.deleteJob(id)
                  location.hash = href("jobs")
                  return "Job deleted"
                },
              })
            }
          >
            <Trash2Icon data-icon="inline-start" />
            Delete
          </Button>
        )}
      </PageHeader>

      <section className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatTile
          label="Tasks done"
          loading={!p}
          value={p && `${p.done}/${p.total}`}
          detail={job && `${job.counts.failed ?? 0} failed · ${job.counts.running ?? 0} running`}
        />
        <StatTile
          label="Elapsed"
          loading={!job}
          value={duration(stats.elapsed)}
          detail={job && `created ${since(job.created_at)}`}
        />
        <StatTile
          label="Attempts"
          loading={!tl.data}
          value={stats.attempts}
          detail={`${stats.failures} failed · ${stats.yields} yielded · ${stats.lost} lost`}
        />
        <StatTile label="Macs used" loading={!tl.data} value={stats.macs} />
      </section>

      <Card size="sm">
        <CardHeader>
          <CardTitle>Timeline</CardTitle>
          <CardDescription>Every attempt of every task, on the Mac that ran it</CardDescription>
          <CardAction>
            <ToggleGroup
              type="single"
              variant="outline"
              size="sm"
              value={by}
              onValueChange={(v) => v && setBy(v as "task" | "mac")}
              aria-label="Group timeline by"
            >
              <ToggleGroupItem value="task" className="px-2 max-md:h-9">
                By task
              </ToggleGroupItem>
              <ToggleGroupItem value="mac" className="px-2 max-md:h-9">
                By Mac
              </ToggleGroupItem>
            </ToggleGroup>
          </CardAction>
        </CardHeader>
        <CardContent>
          {!tl.data ? <Skeleton className="h-40 w-full" /> : <Timeline attempts={tl.data} by={by} now={now} />}
        </CardContent>
      </Card>

      <Card size="sm">
        <CardHeader>
          <CardTitle>Tasks</CardTitle>
          <CardDescription>Open a task for its attempts, logs and placement decision</CardDescription>
        </CardHeader>
        <CardContent>
          {!job ? (
            <Skeleton className="h-32 w-full" />
          ) : mobile ? (
            <ul className="flex flex-col">
              {(job.tasks ?? []).map((t) => (
                <li key={t.id} className="border-b last:border-b-0">
                  <button
                    type="button"
                    className="flex min-h-11 w-full items-center gap-3 py-2 text-left"
                    onClick={() => setTaskId(t.id)}
                  >
                    <div className="min-w-0 flex-1">
                      <div className="truncate text-sm font-medium">{t.key}</div>
                      <div className="truncate text-xs text-muted-foreground">
                        attempt {t.attempt} · {t.failures} failures{t.error ? ` · ${t.error}` : ""}
                      </div>
                    </div>
                    <StateBadge state={t.state} />
                  </button>
                </li>
              ))}
            </ul>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Task</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Runtime</TableHead>
                  <TableHead className="hidden lg:table-cell">Depends on</TableHead>
                  <TableHead>Attempt</TableHead>
                  <TableHead>Failures</TableHead>
                  <TableHead>Mac</TableHead>
                  <TableHead className="hidden lg:table-cell">Last error</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(job.tasks ?? []).map((t) => (
                  <TableRow key={t.id} className="cursor-pointer" onClick={() => setTaskId(t.id)}>
                    <TableCell className="font-medium">{t.key}</TableCell>
                    <TableCell>
                      <StateBadge state={t.state} />
                    </TableCell>
                    <TableCell>
                      <code className="text-xs">{t.spec.runtime}</code>
                    </TableCell>
                    <TableCell className="hidden text-muted-foreground lg:table-cell">
                      {t.spec.depends_on?.join(", ") || "–"}
                    </TableCell>
                    <TableCell className="tabular-nums">{t.attempt}</TableCell>
                    <TableCell className="tabular-nums">{t.failures}</TableCell>
                    <TableCell className="text-xs">
                      {t.agent_id ? (
                        <a href={href("mac", t.agent_id)} className="hover:underline" onClick={(e) => e.stopPropagation()}>
                          {macNames[t.agent_id] ?? t.agent_id}
                        </a>
                      ) : (
                        "–"
                      )}
                    </TableCell>
                    <TableCell className="hidden max-w-64 truncate text-muted-foreground lg:table-cell">
                      {t.error || "–"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
      <TaskPanel key={taskId ?? "none"} id={taskId} onClose={() => setTaskId(undefined)} />
      {confirm}
    </div>
  )
}
