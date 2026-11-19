import {
  ActivityIcon,
  AlertTriangleIcon,
  ArrowDownCircleIcon,
  CircleSlashIcon,
  KeyRoundIcon,
  LaptopIcon,
  LinkIcon,
  ListChecksIcon,
  PauseIcon,
  PlayIcon,
  SettingsIcon,
  Trash2Icon,
  UserIcon,
} from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
} from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { since, type OpsEvent } from "@/lib/api"
import { href, useNow } from "@/lib/hooks"
import { cn } from "@/lib/utils"

// StatTile: label · value · optional detail and trend. One number per tile.
export function StatTile({
  label,
  value,
  detail,
  icon: Icon,
  trend,
  loading,
  link,
}: {
  label: string
  value: React.ReactNode
  detail?: React.ReactNode
  icon?: React.ComponentType<{ className?: string }>
  trend?: React.ReactNode
  loading?: boolean
  link?: string
}) {
  const body = (
    <Card
      size="sm"
      className={cn(
        "h-full gap-1",
        link && "transition-colors hover:bg-muted/50"
      )}
    >
      <CardHeader className="gap-0.5">
        <CardDescription className="flex items-center gap-1.5">
          {Icon && <Icon className="size-3.5" aria-hidden />}
          {label}
        </CardDescription>
        <div className="text-2xl leading-tight font-semibold tabular-nums md:text-3xl">
          {loading ? <Skeleton className="h-8 w-16" /> : value}
        </div>
      </CardHeader>
      {(detail || trend) && (
        <CardContent className="flex flex-col gap-1">
          {detail && (
            <div className="text-xs text-muted-foreground">{detail}</div>
          )}
          {trend}
        </CardContent>
      )}
    </Card>
  )
  return link ? (
    <a
      href={link}
      className="block rounded-xl focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      {body}
    </a>
  ) : (
    body
  )
}

export const ranges = [
  { value: "1h", label: "1h" },
  { value: "6h", label: "6h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
  { value: "30d", label: "30d" },
]

// RangePicker selects the time range of the charts on a page.
export function RangePicker({
  value,
  onChange,
}: {
  value: string
  onChange: (v: string) => void
}) {
  return (
    <ToggleGroup
      type="single"
      variant="outline"
      size="sm"
      value={value}
      onValueChange={(v) => v && onChange(v)}
      aria-label="Time range"
    >
      {ranges.map((r) => (
        <ToggleGroupItem
          key={r.value}
          value={r.value}
          className="min-w-10 px-2 max-md:h-9"
        >
          {r.label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}

// LiveToggle shows when data was last refreshed and pauses auto-refresh.
export function LiveToggle({
  paused,
  onToggle,
  updatedAt,
}: {
  paused: boolean
  onToggle: () => void
  updatedAt?: number
}) {
  const now = useNow(1000)
  const age = updatedAt
    ? Math.max(0, Math.round((now - updatedAt) / 1000))
    : undefined
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground">
      <span aria-live="polite" className="hidden sm:inline">
        {paused
          ? "Paused"
          : age === undefined
            ? "Loading…"
            : age < 2
              ? "Updated just now"
              : `Updated ${age}s ago`}
      </span>
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            variant="outline"
            size="sm"
            onClick={onToggle}
            aria-pressed={!paused}
            className="max-md:h-9"
          >
            {paused ? (
              <PlayIcon data-icon="inline-start" />
            ) : (
              <PauseIcon data-icon="inline-start" />
            )}
            {paused ? "Resume" : "Live"}
          </Button>
        </TooltipTrigger>
        <TooltipContent>
          {paused ? "Resume auto-refresh" : "Pause auto-refresh"}
        </TooltipContent>
      </Tooltip>
    </div>
  )
}

const eventIcons: Record<
  string,
  React.ComponentType<{ className?: string }>
> = {
  mac: LaptopIcon,
  task: ArrowDownCircleIcon,
  job: ListChecksIcon,
  jobs: ListChecksIcon,
  key: KeyRoundIcon,
  install_token: LinkIcon,
  retention: SettingsIcon,
}

function eventIcon(kind: string) {
  if (kind.endsWith(".deleted") || kind === "jobs.purged") return Trash2Icon
  if (
    kind === "mac.quarantined" ||
    kind === "mac.runtime_unavailable" ||
    kind === "mac.fault"
  )
    return AlertTriangleIcon
  if (kind === "key.revoked") return CircleSlashIcon
  return eventIcons[kind.split(".")[0]] ?? ActivityIcon
}

// eventLink points an event at the page of what it is about.
function eventLink(e: OpsEvent): string | undefined {
  if (!e.target) return undefined
  if (e.target.startsWith("mac_") && e.kind !== "mac.deleted")
    return href("mac", e.target)
  if (e.target.startsWith("job_") && e.kind !== "job.deleted")
    return href("job", e.target)
  return undefined
}

// EventList renders activity/audit events as a compact timeline.
export function EventList({
  events,
  loading,
  empty,
}: {
  events?: OpsEvent[]
  loading?: boolean
  empty?: string
}) {
  if (loading || !events) {
    return (
      <div className="flex flex-col gap-3">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    )
  }
  if (events.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        {empty ?? "Nothing has happened yet."}
      </p>
    )
  }
  return (
    <ol className="flex flex-col">
      {events.map((e) => {
        const Icon = eventIcon(e.kind)
        const link = eventLink(e)
        const msg = link ? (
          <a href={link} className="underline-offset-4 hover:underline">
            {e.message}
          </a>
        ) : (
          e.message
        )
        return (
          <li key={e.id} className="flex gap-3 border-b py-2.5 last:border-b-0">
            <span className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-md bg-muted">
              <Icon className="size-3.5 text-muted-foreground" aria-hidden />
            </span>
            <div className="flex min-w-0 flex-1 flex-col gap-0.5">
              <span className="text-sm break-words">{msg}</span>
              <span className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
                <time dateTime={e.at} title={new Date(e.at).toLocaleString()}>
                  {since(e.at)}
                </time>
                <span className="font-mono">{e.kind}</span>
                {e.actor && (
                  <span className="flex items-center gap-1 truncate">
                    <UserIcon className="size-3" aria-hidden />
                    {e.actor}
                  </span>
                )}
              </span>
            </div>
          </li>
        )
      })}
    </ol>
  )
}

// PageHeader: title, description and actions; actions wrap under the title
// on small screens.
export function PageHeader({
  title,
  description,
  children,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  children?: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
      <div className="min-w-0">
        <h2 className="truncate text-xl font-semibold tracking-tight">
          {title}
        </h2>
        {description && (
          <p className="text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {children && (
        <div className="flex flex-wrap items-center gap-2">{children}</div>
      )}
    </div>
  )
}
