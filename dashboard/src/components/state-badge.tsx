import { Badge } from "@/components/ui/badge"

const variants: Record<string, "default" | "secondary" | "destructive" | "outline"> = {
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
  offline: "destructive",
  failed: "destructive",
  timeout: "destructive",
  cancelled: "outline",
}

// StateBadge renders a job/task/Mac/attempt state with the official Badge.
export function StateBadge({ state }: { state: string }) {
  return <Badge variant={variants[state] ?? "outline"}>{state || "unknown"}</Badge>
}
