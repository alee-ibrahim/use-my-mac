import { useCallback, useMemo, useState } from "react"
import {
  ActivityIcon,
  BatteryIcon,
  CheckCircle2Icon,
  CpuIcon,
  GaugeIcon,
  HourglassIcon,
  LaptopIcon,
  MoonIcon,
  PlugIcon,
  ThermometerIcon,
  TimerIcon,
  UndoIcon,
} from "lucide-react"

import { ChartCard, derive, Sparkline, stepLabel, type SeriesDef } from "@/components/charts"
import { EventList, LiveToggle, PageHeader, RangePicker, StatTile } from "@/components/ops"
import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import { api, compact, percent, since, type Mac, type Point } from "@/lib/api"
import { href, usePolling } from "@/lib/hooks"
import { MacStateBadges, PresenceLine, VersionLine } from "@/components/mac-bits"

export const fleetSeries: Record<string, SeriesDef[]> = {
  utilisation: [
    { key: "macs_busy", label: "Busy", color: "var(--chart-1)" },
    { key: "macs_yielded", label: "Yielded", color: "var(--chart-2)" },
    { key: "macs_idle", label: "Idle", color: "var(--chart-3)" },
    { key: "macs_draining", label: "Draining", color: "var(--chart-4)" },
  ],
  throughput: [
    { key: "succeeded", label: "Succeeded", color: "var(--chart-1)" },
    { key: "failures", label: "Failed", color: "var(--chart-2)" },
  ],
  queue: [
    { key: "tasks_queued", label: "Queued", color: "var(--chart-1)" },
    { key: "tasks_running", label: "Running", color: "var(--chart-2)" },
  ],
  yields: [
    { key: "yielded", label: "Yielded", color: "var(--chart-1)" },
    { key: "lost", label: "Lost", color: "var(--chart-2)" },
  ],
}

const sparkSeries: SeriesDef[] = [
  { key: "our_cpu", label: "Our CPU %", color: "var(--chart-1)" },
  { key: "user_cpu", label: "User CPU %", color: "var(--chart-2)" },
]

function round1(p: Point, keys: string[]) {
  const out: Record<string, number> = {}
  for (const k of keys) if (p[k] !== undefined) out[k] = Math.round(p[k]! * 10) / 10
  return out
}

export function OverviewPage({ admin }: { admin: boolean }) {
  const [range, setRange] = useState(() => localStorage.getItem("umm.range") ?? "24h")
  const [paused, setPaused] = useState(false)
  const pickRange = (r: string) => {
    localStorage.setItem("umm.range", r)
    setRange(r)
  }

  const summary = usePolling(api.summary, 5000, { paused })
  const loadSeries = useCallback(() => api.fleetSeries(range), [range])
  const series = usePolling(loadSeries, 30000, { paused })
  const macs = usePolling(api.macs, 5000, { paused })
  const sparks = usePolling(useCallback(() => api.macSparks("1h"), []), 60000, { paused })
  const jobs = usePolling(useCallback(() => api.jobs("", 8), []), 10000, { paused })
  const events = usePolling(useCallback(() => api.events({ limit: 10 }), []), 15000, { paused })

  const s = summary.data
  const pts = useMemo(
    () =>
      derive(series.data?.points, (p) => ({
        failures: (p.failed ?? 0) + (p.timeout ?? 0),
        ...round1(p, ["macs_busy", "macs_yielded", "macs_idle", "macs_draining", "tasks_queued", "tasks_running"]),
      })),
    [series.data]
  )
  const step = stepLabel(series.data?.step_seconds)
  const trend = (key: string) =>
    pts && (
      <Sparkline
        data={pts}
        series={[{ key, label: key.replace("_", " "), color: "var(--chart-1)" }]}
        className="h-8"
      />
    )

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader title="Overview" description="Live state of the Mac pool and what it has done.">
        <RangePicker value={range} onChange={pickRange} />
        <LiveToggle paused={paused} onToggle={() => setPaused((p) => !p)} updatedAt={summary.updatedAt} />
      </PageHeader>

      {summary.error && (
        <Alert variant="destructive">
          <AlertDescription>{summary.error}</AlertDescription>
        </Alert>
      )}

      <section aria-label="Key numbers" className="grid grid-cols-2 gap-3 md:gap-4 lg:grid-cols-4">
        <StatTile
          label="Macs online"
          icon={LaptopIcon}
          loading={!s}
          value={s && `${s.macs.online}/${s.macs.total}`}
          detail={s && `${s.macs.offline} offline · ${s.macs.draining} draining`}
          trend={trend("macs_online")}
          link={href("macs")}
        />
        <StatTile
          label="Idle, ready for work"
          icon={MoonIcon}
          loading={!s}
          value={s?.macs.idle}
          trend={trend("macs_idle")}
        />
        <StatTile label="Busy" icon={CpuIcon} loading={!s} value={s?.macs.busy} trend={trend("macs_busy")} />
        <StatTile
          label="Yielded to their user"
          icon={UndoIcon}
          loading={!s}
          value={s?.macs.yielded}
          trend={trend("macs_yielded")}
        />
        <StatTile
          label="Tasks running"
          icon={ActivityIcon}
          loading={!s}
          value={s?.tasks.running}
          detail={s && `${s.jobs.running} jobs running`}
          trend={trend("tasks_running")}
          link={href("jobs")}
        />
        <StatTile
          label="Tasks queued"
          icon={HourglassIcon}
          loading={!s}
          value={s?.tasks.queued}
          detail={s && `${s.tasks.blocked} waiting on dependencies`}
          trend={trend("tasks_queued")}
        />
        <StatTile
          label="Success rate, 24 h"
          icon={CheckCircle2Icon}
          loading={!s}
          value={s && percent(s.last_24h.success_rate)}
          detail={
            s &&
            `${compact(s.last_24h.succeeded)} succeeded · ${compact(s.last_24h.failed + s.last_24h.timeout)} failed · ${compact(s.last_24h.yielded)} yielded`
          }
        />
        <StatTile
          label="Compute harvested, 24 h"
          icon={TimerIcon}
          loading={!s}
          value={s && `${s.last_24h.compute_hours.toFixed(1)} h`}
          detail={s && `${s.last_7d.compute_hours.toFixed(1)} h in 7 days · task time on Macs`}
        />
      </section>

      <section aria-label="Charts" className="grid gap-3 md:gap-4 lg:grid-cols-2">
        <ChartCard
          title="Fleet utilisation"
          description="Macs by state (average per bucket)"
          data={pts}
          series={fleetSeries.utilisation}
          kind="stacked-area"
        />
        <ChartCard
          title="Throughput"
          description={`Finished attempts ${step}`}
          data={pts}
          series={fleetSeries.throughput}
          kind="stacked-bar"
        />
        <ChartCard
          title="Queue depth"
          description="Tasks waiting for a Mac vs. running"
          data={pts}
          series={fleetSeries.queue}
          kind="line"
        />
        <ChartCard
          title="Yields and lost attempts"
          description={`Tasks handed back ${step}; requeued without counting a failure`}
          data={pts}
          series={fleetSeries.yields}
          kind="stacked-bar"
        />
      </section>
      {series.error && (
        <Alert variant="destructive">
          <AlertDescription>{series.error}</AlertDescription>
        </Alert>
      )}

      <section aria-label="Macs" className="flex flex-col gap-3">
        <div className="flex items-center justify-between">
          <h3 className="text-base font-medium">Macs</h3>
          <Button variant="ghost" size="sm" asChild>
            <a href={href("macs")}>All Macs</a>
          </Button>
        </div>
        {!macs.data ? (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-44 rounded-xl" />
            ))}
          </div>
        ) : macs.data.length === 0 ? (
          <Empty className="border">
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <LaptopIcon />
              </EmptyMedia>
              <EmptyTitle>No Macs yet</EmptyTitle>
              <EmptyDescription>
                {admin ? (
                  <a href={href("macs")}>Add a Mac</a>
                ) : (
                  "An admin adds Macs from the Macs page."
                )}
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {macs.data.map((m) => (
              <MacLiveCard key={m.id} m={m} spark={sparks.data?.macs[m.id]} />
            ))}
          </div>
        )}
      </section>

      <section className="grid gap-3 md:gap-4 lg:grid-cols-2">
        <Card size="sm">
          <CardHeader>
            <CardTitle>Recent jobs</CardTitle>
            <CardAction>
              <Button variant="ghost" size="sm" asChild>
                <a href={href("jobs")}>All jobs</a>
              </Button>
            </CardAction>
          </CardHeader>
          <CardContent>
            {!jobs.data ? (
              <Skeleton className="h-40 w-full" />
            ) : jobs.data.length === 0 ? (
              <p className="text-sm text-muted-foreground">No jobs yet.</p>
            ) : (
              <ul className="flex flex-col">
                {jobs.data.map((j) => {
                  const total = Object.values(j.counts).reduce((a, b) => a + b, 0)
                  const done = (j.counts.succeeded ?? 0) + (j.counts.failed ?? 0) + (j.counts.cancelled ?? 0)
                  return (
                    <li key={j.id} className="border-b last:border-b-0">
                      <a
                        href={href("job", j.id)}
                        className="flex min-h-11 items-center gap-3 py-2 hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-none"
                      >
                        <div className="min-w-0 flex-1">
                          <div className="truncate text-sm font-medium">{j.name || j.id}</div>
                          <div className="text-xs text-muted-foreground">
                            {done}/{total} tasks · {since(j.created_at)}
                          </div>
                        </div>
                        <StateBadge state={j.state} />
                      </a>
                    </li>
                  )
                })}
              </ul>
            )}
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardTitle>Recent events</CardTitle>
            <CardDescription>Enrolments, yields, failures, updates and admin changes</CardDescription>
            <CardAction>
              <Button variant="ghost" size="sm" asChild>
                <a href={href("activity")}>All activity</a>
              </Button>
            </CardAction>
          </CardHeader>
          <CardContent>
            <EventList events={events.data} />
          </CardContent>
        </Card>
      </section>
    </div>
  )
}

function MiniMeter({ label, value }: { label: string; value?: number }) {
  return (
    <div className="flex flex-col gap-1">
      <div className="flex justify-between text-xs">
        <span className="text-muted-foreground">{label}</span>
        <span className="tabular-nums">{value === undefined || value < 0 ? "–" : `${Math.round(value)}%`}</span>
      </div>
      <Progress value={value === undefined || value < 0 ? 0 : Math.min(100, value)} aria-label={label} />
    </div>
  )
}

function MacLiveCard({ m, spark }: { m: Mac; spark?: Point[] }) {
  const t = m.online ? m.telemetry : undefined
  const mem = t?.mem_total_bytes ? (100 * (t.mem_total_bytes - t.mem_available_bytes)) / t.mem_total_bytes : undefined
  return (
    <a
      href={href("mac", m.id)}
      className="block rounded-xl focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      <Card size="sm" className="h-full transition-colors hover:bg-muted/40">
        <CardHeader>
          <CardTitle className="truncate">{m.name}</CardTitle>
          <CardDescription className="truncate">
            {[t?.chip, t?.cpu_count ? `${t.cpu_count} cores` : ""].filter(Boolean).join(" · ") || m.id}
          </CardDescription>
          <CardAction>
            <MacStateBadges m={m} />
          </CardAction>
        </CardHeader>
        <CardContent className="flex flex-col gap-3 text-sm">
          <PresenceLine m={m} />
          {spark && spark.some((p) => p.our_cpu !== undefined) ? (
            <div>
              <div className="flex justify-between text-xs text-muted-foreground">
                <span>CPU, last hour (ours + user)</span>
                <span className="tabular-nums">
                  {t ? `${Math.round(t.our_cpu_percent)}% + ${Math.round(t.user_cpu_percent)}%` : ""}
                </span>
              </div>
              <Sparkline data={spark} series={sparkSeries} max={100} />
            </div>
          ) : (
            <div className="h-14" />
          )}
          <div className="grid grid-cols-2 gap-3">
            <MiniMeter label="GPU" value={t?.gpu_util_percent} />
            <MiniMeter label="Memory" value={mem} />
          </div>
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
            <span className="flex items-center gap-1">
              <GaugeIcon className="size-3.5" aria-hidden />
              {m.running.length}/{m.max_tasks} tasks
            </span>
            {t && (
              <span className="flex items-center gap-1">
                <ThermometerIcon className="size-3.5" aria-hidden />
                {t.thermal || "–"}
              </span>
            )}
            {t && (
              <span className="flex items-center gap-1">
                {t.on_ac ? (
                  <PlugIcon className="size-3.5" aria-hidden />
                ) : (
                  <BatteryIcon className="size-3.5" aria-hidden />
                )}
                {t.on_ac ? "AC" : t.battery_percent >= 0 ? `${t.battery_percent}%` : "battery"}
              </span>
            )}
            <VersionLine m={m} />
          </div>
        </CardContent>
      </Card>
    </a>
  )
}
