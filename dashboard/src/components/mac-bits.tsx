import { LockIcon, UserIcon } from "lucide-react"

import { StateBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import { duration, since, type Mac } from "@/lib/api"

// Presence: who is at the Mac. A logged-in user never stops work by itself.
export function PresenceLine({ m }: { m: Mac }) {
  const t = m.telemetry
  if (!m.online || !t) return <span className="text-muted-foreground">offline · seen {since(m.last_seen)}</span>
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
        {t.screen_locked ? "locked" : t.user_idle_seconds >= 60 ? `idle ${duration(t.user_idle_seconds)}` : "active"}
      </span>
    </span>
  )
}

export function MacStateBadges({ m }: { m: Mac }) {
  return (
    <div className="flex flex-wrap gap-1">
      <StateBadge state={m.agent_state} />
      {m.drain && <Badge variant="outline">drained</Badge>}
      {m.opted_out && <Badge variant="outline">opted out</Badge>}
      {m.quarantined_until && <Badge variant="destructive">quarantined</Badge>}
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
      {u?.state && !["up_to_date", "succeeded"].includes(u.state) && <StateBadge state={u.state} />}
    </span>
  )
}
