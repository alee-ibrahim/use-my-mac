import { Badge } from "@/components/ui/badge"

const variants: Record<
  string,
  "default" | "secondary" | "destructive" | "outline"
> = {
  ready: "default",
  succeeded: "default",
  running: "secondary",
  assigned: "secondary",
  queued: "outline",
  pending: "outline",
  blocked: "outline",
  cooldown: "outline",
  draining: "outline",
  yielding: "secondary",
  yielded: "secondary",
  lost: "secondary",
  unavailable: "secondary",
  offline: "destructive",
  unhealthy: "destructive",
  failed: "destructive",
  timeout: "destructive",
  cancelled: "outline",
  // agent self-update
  up_to_date: "outline",
  paused: "outline",
  downloading: "secondary",
  applying: "secondary",
  rolled_back: "destructive",
  refused: "destructive",
  // install links
  active: "default",
  used: "secondary",
  expired: "outline",
  revoked: "outline",
}

// StateBadge renders a job/task/Mac/attempt state with the official Badge.
export function StateBadge({ state }: { state: string }) {
  return (
    <Badge variant={variants[state] ?? "outline"}>
      {state ? state.replaceAll("_", " ") : "unknown"}
    </Badge>
  )
}
