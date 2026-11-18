import { useMemo } from "react"
import { Area, AreaChart, Bar, BarChart, CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts"

import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import type { Point } from "@/lib/api"
import { cn } from "@/lib/utils"

// Series colours come from the validated categorical order in index.css.
// Assign slots in order and never by rank, so a series keeps its colour.
export type SeriesDef = { key: string; label: string; color: string }

export type ChartKind = "area" | "stacked-area" | "bar" | "stacked-bar" | "line"

function timeFormatter(spanMs: number) {
  if (spanMs <= 36 * 3600_000) {
    return (t: number) => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
  }
  return (t: number) => new Date(t).toLocaleDateString([], { month: "short", day: "numeric" })
}

function fullTime(t: number) {
  return new Date(t).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })
}

function tickNumber(v: number, unit?: string) {
  const s = Math.abs(v) >= 1000 ? `${(v / 1000).toFixed(v % 1000 ? 1 : 0)}K` : `${+v.toFixed(1)}`
  return unit === "%" ? `${s}%` : s
}

// TimeChart draws one time series chart (one y-axis, always).
export function TimeChart({
  data,
  series,
  kind,
  unit,
  yMax,
  className,
  legend = series.length > 1,
}: {
  data: Point[]
  series: SeriesDef[]
  kind: ChartKind
  unit?: string
  yMax?: number
  className?: string
  legend?: boolean
}) {
  const config = useMemo(
    () => Object.fromEntries(series.map((s) => [s.key, { label: s.label, color: s.color }])) as ChartConfig,
    [series]
  )
  const span = data.length > 1 ? data[data.length - 1].t - data[0].t : 0
  const fmt = timeFormatter(span)
  const common = {
    data,
    margin: { left: 0, right: 8, top: 8, bottom: 0 },
    accessibilityLayer: true,
  }
  const axes = (
    <>
      <CartesianGrid vertical={false} />
      {/* Bars need a band (category) axis to get a width; lines and areas use a time axis. */}
      <XAxis
        dataKey="t"
        {...(kind.includes("bar")
          ? { type: "category" as const }
          : { type: "number" as const, scale: "time" as const, domain: ["dataMin", "dataMax"] })}
        tickFormatter={fmt}
        tickLine={false}
        axisLine={false}
        tickMargin={8}
        minTickGap={40}
      />
      <YAxis
        width={44}
        tickLine={false}
        axisLine={false}
        tickMargin={4}
        allowDecimals={false}
        domain={yMax ? [0, yMax] : [0, "auto"]}
        tickFormatter={(v: number) => tickNumber(v, unit)}
      />
      <ChartTooltip
        cursor={kind.includes("bar") ? { fillOpacity: 0.5 } : true}
        content={
          <ChartTooltipContent
            indicator={kind === "line" ? "line" : "dot"}
            labelFormatter={(_, p) => (p?.[0] ? fullTime(p[0].payload.t) : "")}
          />
        }
      />
      {legend && <ChartLegend itemSorter={null} content={<ChartLegendContent />} />}
    </>
  )
  const cls = cn("aspect-auto h-56 w-full", className)

  if (kind === "bar" || kind === "stacked-bar") {
    const stacked = kind === "stacked-bar"
    return (
      <ChartContainer config={config} className={cls}>
        <BarChart {...common} barCategoryGap="15%">
          {axes}
          {series.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={s.key}
              stackId={stacked ? "a" : undefined}
              fill={`var(--color-${s.key})`}
              maxBarSize={24}
              stroke="var(--color-card)"
              strokeWidth={stacked && data.length <= 60 ? 1 : 0}
              radius={!stacked || i === series.length - 1 ? [4, 4, 0, 0] : 0}
              isAnimationActive={false}
            />
          ))}
        </BarChart>
      </ChartContainer>
    )
  }
  if (kind === "line") {
    return (
      <ChartContainer config={config} className={cls}>
        <LineChart {...common}>
          {axes}
          {series.map((s) => (
            <Line
              key={s.key}
              dataKey={s.key}
              type="monotone"
              stroke={`var(--color-${s.key})`}
              strokeWidth={2}
              dot={false}
              connectNulls={false}
              isAnimationActive={false}
            />
          ))}
        </LineChart>
      </ChartContainer>
    )
  }
  const stacked = kind === "stacked-area"
  return (
    <ChartContainer config={config} className={cls}>
      <AreaChart {...common}>
        {axes}
        {series.map((s) => (
          <Area
            key={s.key}
            dataKey={s.key}
            type="monotone"
            stackId={stacked ? "a" : undefined}
            stroke={`var(--color-${s.key})`}
            strokeWidth={2}
            fill={`var(--color-${s.key})`}
            fillOpacity={stacked ? 0.2 : 0.1}
            connectNulls={false}
            isAnimationActive={false}
          />
        ))}
      </AreaChart>
    </ChartContainer>
  )
}

// Sparkline: a tiny trend without axes (the card around it names it).
export function Sparkline({
  data,
  series,
  max,
  className,
}: {
  data: Point[]
  series: SeriesDef[]
  max?: number
  className?: string
}) {
  const config = useMemo(
    () => Object.fromEntries(series.map((s) => [s.key, { label: s.label, color: s.color }])) as ChartConfig,
    [series]
  )
  return (
    <ChartContainer config={config} className={cn("aspect-auto h-10 w-full", className)}>
      <AreaChart data={data} margin={{ top: 2, bottom: 2, left: 0, right: 0 }}>
        <YAxis hide domain={[0, max ?? "auto"]} />
        <XAxis dataKey="t" hide type="number" domain={["dataMin", "dataMax"]} />
        <ChartTooltip
          cursor={false}
          content={
            <ChartTooltipContent
              indicator="line"
              labelFormatter={(_, p) => (p?.[0] ? fullTime(p[0].payload.t) : "")}
            />
          }
        />
        {series.map((s) => (
          <Area
            key={s.key}
            dataKey={s.key}
            type="monotone"
            stackId="a"
            stroke={`var(--color-${s.key})`}
            strokeWidth={1.5}
            fill={`var(--color-${s.key})`}
            fillOpacity={0.15}
            isAnimationActive={false}
          />
        ))}
      </AreaChart>
    </ChartContainer>
  )
}

// ChartCard frames a chart with a title, an optional headline and a table
// view of the same data (so nothing is colour-only or hover-only).
export function ChartCard({
  title,
  description,
  headline,
  data,
  series,
  kind,
  unit,
  yMax,
  action,
  loading,
}: {
  title: string
  description?: React.ReactNode
  headline?: React.ReactNode
  data?: Point[]
  series: SeriesDef[]
  kind: ChartKind
  unit?: string
  yMax?: number
  action?: React.ReactNode
  loading?: boolean
}) {
  const rows = useMemo(
    () =>
      (data ?? [])
        .filter((p) => series.some((s) => p[s.key] !== undefined))
        .slice(-24)
        .reverse(),
    [data, series]
  )
  return (
    <Card size="sm" className="min-w-0">
      <Tabs defaultValue="chart" className="gap-0">
        <CardHeader>
          <CardTitle>{title}</CardTitle>
          {description && <CardDescription>{description}</CardDescription>}
          <CardAction className="flex items-center gap-2">
            {action}
            <TabsList className="h-7">
              <TabsTrigger value="chart" className="px-2 text-xs">
                Chart
              </TabsTrigger>
              <TabsTrigger value="table" className="px-2 text-xs">
                Table
              </TabsTrigger>
            </TabsList>
          </CardAction>
          {headline && <div className="text-2xl font-semibold tabular-nums">{headline}</div>}
        </CardHeader>
        <CardContent className="pt-3">
          <TabsContent value="chart">
            {loading || !data ? (
              <Skeleton className="h-56 w-full" />
            ) : (
              <TimeChart data={data} series={series} kind={kind} unit={unit} yMax={yMax} />
            )}
          </TabsContent>
          <TabsContent value="table">
            <div className="max-h-56 overflow-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Time</TableHead>
                    {series.map((s) => (
                      <TableHead key={s.key} className="text-right">
                        {s.label}
                      </TableHead>
                    ))}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((p) => (
                    <TableRow key={p.t}>
                      <TableCell className="text-xs whitespace-nowrap">{fullTime(p.t)}</TableCell>
                      {series.map((s) => (
                        <TableCell key={s.key} className="text-right tabular-nums">
                          {p[s.key] === undefined ? "–" : `${+p[s.key]!.toFixed(1)}${unit === "%" ? "%" : ""}`}
                        </TableCell>
                      ))}
                    </TableRow>
                  ))}
                  {rows.length === 0 && (
                    <TableRow>
                      <TableCell colSpan={series.length + 1} className="text-muted-foreground">
                        No data in this range yet.
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </div>
          </TabsContent>
        </CardContent>
      </Tabs>
    </Card>
  )
}

// derive adds computed fields to each point that has data.
export function derive(points: Point[] | undefined, f: (p: Point) => Record<string, number>): Point[] | undefined {
  return points?.map((p) => (Object.keys(p).length > 1 ? { ...p, ...f(p) } : p))
}

// stepLabel describes a bucket size: "per 5 min".
export function stepLabel(s?: number) {
  if (!s) return ""
  return s < 3600 ? `per ${Math.round(s / 60)} min` : `per ${+(s / 3600).toFixed(1)} h`
}
