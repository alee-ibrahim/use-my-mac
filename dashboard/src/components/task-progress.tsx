import { Progress } from "@/components/ui/progress"
import { bytes, terminal, type Task } from "@/lib/api"

// progressText describes a task's live progress: the phase plus what the
// Mac reported (percent of the run, or bytes and rate of a transfer).
export function progressText(t: Task): string {
  const p = t.progress
  if (!p || terminal(t.state)) return t.phase && t.phase !== "done" ? t.phase : ""
  if (p.phase === "running") return p.percent >= 0 ? `running ${Math.floor(p.percent)}%` : "running"
  const parts = [p.phase]
  if (p.bytes_total) parts.push(`${bytes(p.bytes_done)} of ${bytes(p.bytes_total)}`)
  if (p.bytes_per_sec) parts.push(`${bytes(p.bytes_per_sec)}/s`)
  return parts.join(" · ")
}

// TaskProgress: a bar with the task's overall percent and a short label.
// Finished tasks show nothing (their state badge says it all).
export function TaskProgress({ task }: { task: Task }) {
  if (terminal(task.state)) return <span className="text-muted-foreground">–</span>
  const text = progressText(task)
  return (
    <div className="flex min-w-36 flex-col gap-1">
      <Progress value={task.percent ?? 0} aria-label={`task ${task.key} progress`} />
      <span className="truncate text-xs text-muted-foreground tabular-nums" title={task.progress?.message}>
        {text || "–"}
        {task.progress?.message ? ` · ${task.progress.message}` : ""}
      </span>
    </div>
  )
}
