import { useCallback, useMemo, useState } from "react"
import { ArrowLeftIcon } from "lucide-react"

import {
  ChartCard,
  derive,
  stepLabel,
  type SeriesDef,
} from "@/components/charts"
import {
  MacStateBadges,
  PresenceLine,
  RuntimeBadges,
  VersionLine,
} from "@/components/mac-bits"
import {
  EventList,
  LiveToggle,
  PageHeader,
  RangePicker,
} from "@/components/ops"
import { MacPlacementCard } from "@/components/mac-placement"
import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useIsMobile } from "@/hooks/use-mobile"
import { api, bytes, duration, since, type MacAttempt } from "@/lib/api"
import { href, usePolling } from "@/lib/hooks"
import { MacActions } from "@/pages/macs"

const cpu: SeriesDef[] = [
  { key: "our_cpu", label: "Our tasks", color: "var(--chart-1)" },
  { key: "user_cpu", label: "Mac's user", color: "var(--chart-2)" },
]
const gpuMem: SeriesDef[] = [
  { key: "gpu", label: "GPU", color: "var(--chart-1)" },
  { key: "mem_used", label: "Memory used", color: "var(--chart-2)" },
]
const work: SeriesDef[] = [
  { key: "running", label: "Tasks running", color: "var(--chart-1)" },
]
const avail: SeriesDef[] = [
  { key: "online_pct", label: "Online", color: "var(--chart-1)" },
  { key: "yielded_pct", label: "Yielded to its user", color: "var(--chart-2)" },
]

function dur(a: { started_at: string; finished_at?: string }) {
  const end = a.finished_at ? new Date(a.finished_at).getTime() : Date.now()
  return duration((end - new Date(a.started_at).getTime()) / 1000)
}

export function MacDetailPage({ id, admin }: { id: string; admin: boolean }) {
  const [range, setRange] = useState(
    () => localStorage.getItem("umm.range") ?? "24h"
  )
  const [paused, setPaused] = useState(false)
  const macs = usePolling(api.macs, 5000, { paused })
  const m = macs.data?.find((x) => x.id === id)
  const series = usePolling(
    useCallback(() => api.macSeries(id, range), [id, range]),
    30000,
    { paused }
  )
  const attempts = usePolling(
    useCallback(() => api.macAttempts(id, 100), [id]),
    15000,
    { paused }
  )
  const events = usePolling(
    useCallback(() => api.events({ target: id, limit: 30 }), [id]),
    30000,
    { paused }
  )
  const updates = usePolling(
    useCallback(() => api.macUpdates(id), [id]),
    60000,
    { paused }
  )
  const pts = useMemo(
    () =>
      derive(series.data?.points, (p) => ({
        running: Math.round((p.running ?? 0) * 10) / 10,
        online_pct: Math.round((p.online ?? 0) * 100),
        yielded_pct: Math.round((p.yielded ?? 0) * 100),
      })),
    [series.data]
  )
  const t = m?.online ? m.telemetry : undefined
  const last = m?.telemetry

  if (macs.data && !m) {
    return (
      <div className="flex flex-col gap-4">
        <BackLink />
        <Alert>
          <AlertDescription>
            This Mac does not exist (any more).
          </AlertDescription>
        </Alert>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <BackLink />
      <PageHeader
        title={m?.name ?? <Skeleton className="h-7 w-40" />}
        description={m && <span className="font-mono text-xs">{m.id}</span>}
      >
        <RangePicker
          value={range}
          onChange={(r) => (localStorage.setItem("umm.range", r), setRange(r))}
        />
        <LiveToggle
          paused={paused}
          onToggle={() => setPaused((p) => !p)}
          updatedAt={macs.updatedAt}
        />
        {admin && m && (
          <MacActions
            m={m}
            onChanged={() => void macs.refresh()}
            onDeleted={() => (location.hash = href("macs"))}
          />
        )}
      </PageHeader>

      <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Card size="sm">
          <CardHeader>
            <CardDescription>State</CardDescription>
            <CardTitle>
              {m ? <MacStateBadges m={m} /> : <Skeleton className="h-5 w-20" />}
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-1 text-sm">
            {m && <PresenceLine m={m} />}
            <span className="text-muted-foreground">
              {m?.running.length ?? 0}/{m?.max_tasks ?? 0} tasks · last
              heartbeat {since(m?.last_seen)}
            </span>
            {m?.agent_state === "unhealthy" && m.state_reason && (
              <span className="text-destructive">{m.state_reason}</span>
            )}
            {m?.quarantined_until && (
              <span className="text-destructive">
                Quarantined until{" "}
                {new Date(m.quarantined_until).toLocaleTimeString()}
              </span>
            )}
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardDescription>Hardware</CardDescription>
            <CardTitle className="truncate">
              {last?.chip || last?.model || "–"}
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-1 text-sm text-muted-foreground">
            <span>
              {last?.cpu_count ?? "–"} CPU cores · {last?.gpu_cores || "–"} GPU
              cores
            </span>
            <span>
              {bytes(last?.mem_total_bytes)} memory ·{" "}
              {bytes(last?.disk_free_bytes)} disk free
            </span>
            <span>{last?.os || "–"}</span>
            {m?.isolation && (
              <span>
                Work space {bytes(m.isolation.work_space_bytes)} free of{" "}
                {bytes(m.isolation.work_capacity_bytes)} ·{" "}
                {m.isolation.work_volume === "dedicated"
                  ? "own volume"
                  : "system volume"}
                {m.isolation.reserve_bytes
                  ? ` · ${bytes(m.isolation.reserve_bytes)} kept for the user`
                  : ""}
              </span>
            )}
            {m?.isolation && (
              <span>
                Network {m.isolation.network || "allow"} · home folders{" "}
                {m.isolation.home_dirs || "unchecked"}
                {m.isolation.work_volume_note
                  ? ` · ${m.isolation.work_volume_note}`
                  : ""}
              </span>
            )}
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardDescription>Now</CardDescription>
            <CardTitle className="tabular-nums">
              {t
                ? `${Math.round(t.our_cpu_percent)}% ours · ${Math.round(t.user_cpu_percent)}% user`
                : "offline"}
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-1 text-sm text-muted-foreground">
            <span>
              GPU{" "}
              {t && t.gpu_util_percent >= 0
                ? `${Math.round(t.gpu_util_percent)}%`
                : "–"}{" "}
              · memory pressure {t?.memory_pressure || "–"}
            </span>
            <span>
              Thermal {t?.thermal || "–"} ·{" "}
              {t
                ? t.on_ac
                  ? "on AC"
                  : `battery ${t.battery_percent >= 0 ? `${t.battery_percent}%` : ""}`
                : "–"}
            </span>
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardDescription>Agent</CardDescription>
            <CardTitle>{m ? <VersionLine m={m} /> : "–"}</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-1 text-sm text-muted-foreground">
            <span>
              {m?.update?.checked_at
                ? `Updater checked ${since(m.update.checked_at)}`
                : "Updater has not checked in"}
            </span>
            {m?.update?.warning && (
              <span className="text-destructive">{m.update.warning}</span>
            )}
            <span className="flex flex-wrap items-center gap-1">
              Runtimes: {m ? <RuntimeBadges m={m} /> : "–"}
            </span>
            {m && Object.keys(m.labels ?? {}).length > 0 && (
              <span className="flex flex-wrap gap-1">
                {Object.entries(m.labels).map(([k, v]) => (
                  <Badge key={k} variant="outline">
                    {k}={v}
                  </Badge>
                ))}
              </span>
            )}
          </CardContent>
        </Card>
      </section>

      {m && (
        <MacPlacementCard
          m={m}
          admin={admin}
          onChanged={() => void macs.refresh()}
        />
      )}

      {series.error && (
        <Alert variant="destructive">
          <AlertDescription>{series.error}</AlertDescription>
        </Alert>
      )}
      <section
        aria-label="Charts"
        className="grid gap-3 md:gap-4 lg:grid-cols-2"
      >
        <ChartCard
          title="CPU"
          description="Share of the whole Mac: our tasks and the Mac's own user"
          data={pts}
          series={cpu}
          kind="stacked-area"
          unit="%"
          yMax={100}
        />
        <ChartCard
          title="GPU and memory"
          description="GPU utilisation and memory in use"
          data={pts}
          series={gpuMem}
          kind="line"
          unit="%"
          yMax={100}
        />
        <ChartCard
          title="Tasks running"
          description={`Average, ${stepLabel(series.data?.step_seconds)}`}
          data={pts}
          series={work}
          kind="area"
        />
        <ChartCard
          title="Availability"
          description="Share of time online, and yielded to the Mac's user"
          data={pts}
          series={avail}
          kind="line"
          unit="%"
          yMax={100}
        />
      </section>

      <Card size="sm">
        <Tabs defaultValue="tasks" className="gap-0">
          <CardHeader>
            <TabsList>
              <TabsTrigger value="tasks">Task history</TabsTrigger>
              <TabsTrigger value="events">Events</TabsTrigger>
              <TabsTrigger value="updates">Updates</TabsTrigger>
            </TabsList>
          </CardHeader>
          <CardContent className="pt-3">
            <TabsContent value="tasks">
              <AttemptHistory attempts={attempts.data} />
            </TabsContent>
            <TabsContent value="events">
              <EventList
                events={events.data}
                empty="No events for this Mac yet."
              />
            </TabsContent>
            <TabsContent value="updates">
              {!updates.data ? (
                <Skeleton className="h-24 w-full" />
              ) : updates.data.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No update attempts yet.
                </p>
              ) : (
                <ul className="flex flex-col">
                  {updates.data.map((e, i) => (
                    <li
                      key={i}
                      className="flex flex-wrap items-center gap-2 border-b py-2 text-sm last:border-b-0"
                    >
                      <StateBadge state={e.state} />
                      <span className="font-mono text-xs">
                        {e.from_version}
                        {e.to_version && e.to_version !== e.from_version
                          ? ` → ${e.to_version}`
                          : ""}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {new Date(e.at).toLocaleString()}
                      </span>
                      {e.message && (
                        <span className="w-full text-xs text-muted-foreground">
                          {e.message}
                        </span>
                      )}
                    </li>
                  ))}
                </ul>
              )}
            </TabsContent>
          </CardContent>
        </Tabs>
      </Card>
    </div>
  )
}

function BackLink() {
  return (
    <Button
      variant="ghost"
      size="sm"
      className="self-start max-md:h-10"
      asChild
    >
      <a href={href("macs")}>
        <ArrowLeftIcon data-icon="inline-start" />
        Macs
      </a>
    </Button>
  )
}

function AttemptHistory({ attempts }: { attempts?: MacAttempt[] }) {
  const mobile = useIsMobile()
  if (!attempts) return <Skeleton className="h-32 w-full" />
  if (attempts.length === 0)
    return (
      <p className="text-sm text-muted-foreground">
        No tasks have run here yet.
      </p>
    )
  if (mobile) {
    return (
      <ul className="flex flex-col">
        {attempts.map((a) => (
          <li
            key={`${a.task_id}/${a.attempt}`}
            className="border-b last:border-b-0"
          >
            <a
              href={href("job", a.job_id)}
              className="flex min-h-11 items-center gap-3 py-2"
            >
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium">
                  {a.job_name || a.job_id} · {a.task_key}
                </div>
                <div className="text-xs text-muted-foreground">
                  attempt {a.attempt} · {since(a.started_at)} · {dur(a)}
                </div>
              </div>
              <StateBadge state={a.outcome} />
            </a>
          </li>
        ))}
      </ul>
    )
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Job</TableHead>
          <TableHead>Task</TableHead>
          <TableHead>Attempt</TableHead>
          <TableHead>Outcome</TableHead>
          <TableHead>Started</TableHead>
          <TableHead>Duration</TableHead>
          <TableHead>Detail</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {attempts.map((a) => (
          <TableRow key={`${a.task_id}/${a.attempt}`}>
            <TableCell>
              <a
                href={href("job", a.job_id)}
                className="font-medium hover:underline"
              >
                {a.job_name || a.job_id}
              </a>
            </TableCell>
            <TableCell>{a.task_key}</TableCell>
            <TableCell className="tabular-nums">{a.attempt}</TableCell>
            <TableCell>
              <StateBadge state={a.outcome} />
            </TableCell>
            <TableCell className="text-muted-foreground">
              {since(a.started_at)}
            </TableCell>
            <TableCell className="tabular-nums">{dur(a)}</TableCell>
            <TableCell className="max-w-64 truncate text-muted-foreground">
              {a.error}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
