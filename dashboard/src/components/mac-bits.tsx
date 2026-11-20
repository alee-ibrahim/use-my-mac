import { LockIcon, UserIcon } from "lucide-react"

import { StateBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { duration, since, type Mac } from "@/lib/api"

// Presence: who is at the Mac. A logged-in user never stops work by itself.
export function PresenceLine({ m }: { m: Mac }) {
  const t = m.telemetry
  if (!m.online || !t)
    return (
      <span className="text-muted-foreground">
        offline · seen {since(m.last_seen)}
      </span>
    )
  if (!t.console_user)
    return (
      <span className="flex items-center gap-1.5">
        <UserIcon className="size-3.5 text-muted-foreground" aria-hidden />
        nobody logged in
      </span>
    )
  return (
    <span className="flex items-center gap-1.5">
      {t.screen_locked ? (
        <LockIcon className="size-3.5 text-muted-foreground" aria-hidden />
      ) : (
        <UserIcon className="size-3.5 text-muted-foreground" aria-hidden />
      )}
      <span className="truncate">{t.console_user}</span>
      <span className="text-muted-foreground">
        {t.screen_locked
          ? "locked"
          : t.user_idle_seconds >= 60
            ? `idle ${duration(t.user_idle_seconds)}`
            : "active"}
      </span>
    </span>
  )
}

export function MacStateBadges({ m }: { m: Mac }) {
  return (
    <div className="flex flex-wrap gap-1">
      {m.state_reason && m.agent_state !== "ready" ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              tabIndex={0}
              aria-label={`${m.agent_state}: ${m.state_reason}`}
              onClick={(e) => e.stopPropagation()}
            >
              <StateBadge state={m.agent_state} />
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-96">{m.state_reason}</TooltipContent>
        </Tooltip>
      ) : (
        <StateBadge state={m.agent_state} />
      )}
      {m.drain && <Badge variant="outline">drained</Badge>}
      {m.opted_out && <Badge variant="outline">opted out</Badge>}
      {m.quarantined_until && <Badge variant="destructive">quarantined</Badge>}
      {m.reservation && (
        <Badge variant="secondary">
          {m.reservation.mine ? "reserved for you" : "reserved"}
        </Badge>
      )}
    </div>
  )
}

// RuntimeBadges: the runtimes a Mac can run, then the allowlisted ones it cannot
// (muted and struck through; the tooltip says why).
export function RuntimeBadges({ m }: { m: Mac }) {
  const bad = m.unavailable_runtimes ?? []
  if (!m.runtimes?.length && !bad.length)
    return <span className="text-muted-foreground">–</span>
  return (
    <div className="flex flex-wrap gap-1">
      {(m.runtimes ?? []).map((r) => (
        <Badge key={r} variant="outline">
          {r}
        </Badge>
      ))}
      {bad.map((u) => (
        <Tooltip key={u.name}>
          <TooltipTrigger asChild>
            <Badge
              variant="outline"
              tabIndex={0}
              className="text-muted-foreground line-through"
              aria-label={`${u.name} unavailable: ${u.reason}`}
              onClick={(e) => e.stopPropagation()}
            >
              {u.name}
            </Badge>
          </TooltipTrigger>
          <TooltipContent className="max-w-80">
            Unavailable: {u.reason}
            {u.source === "exec"
              ? " (a task could not start it; retried after the next preflight)"
              : ""}
            {u.since ? ` · since ${since(u.since)}` : ""}
          </TooltipContent>
        </Tooltip>
      ))}
    </div>
  )
}

export function VersionLine({ m }: { m: Mac }) {
  const u = m.update
  const pending = u?.desired && m.version && u.desired !== m.version
  return (
    <span className="flex items-center gap-1.5 font-mono text-xs">
      {m.version || "–"}
      {pending && <span className="text-muted-foreground">→ {u?.desired}</span>}
      {u?.state && !["up_to_date", "succeeded"].includes(u.state) && (
        <StateBadge state={u.state} />
      )}
      {u?.warning && (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="destructive">updater not running</Badge>
          </TooltipTrigger>
          <TooltipContent className="max-w-80">{u.warning}</TooltipContent>
        </Tooltip>
      )}
    </span>
  )
}
