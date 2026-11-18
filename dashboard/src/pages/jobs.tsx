import { useEffect, useState } from "react"
import { toast } from "sonner"
import { ArrowLeftIcon, ListChecksIcon, RefreshCwIcon } from "lucide-react"

import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Progress } from "@/components/ui/progress"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { api, since, type Job, type Task } from "@/lib/api"
import { usePolling } from "@/lib/hooks"

function progress(j: Job) {
  const total = Object.values(j.counts).reduce((a, b) => a + b, 0)
  const done = (j.counts.succeeded ?? 0) + (j.counts.failed ?? 0) + (j.counts.cancelled ?? 0)
  return { total, done, pct: total ? (100 * done) / total : 0 }
}

function JobsList({ onOpen }: { onOpen: (id: string) => void }) {
  const { data, error } = usePolling(api.jobs, 3000)
  return (
    <Card>
      <CardHeader>
        <CardTitle>Jobs</CardTitle>
        <CardDescription>Most recent first. Click a job to see its tasks, retries and logs.</CardDescription>
      </CardHeader>
      <CardContent>
        {error && (
          <Alert variant="destructive" className="mb-4">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {data && data.length === 0 ? (
          <Empty>
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <ListChecksIcon />
              </EmptyMedia>
              <EmptyTitle>No jobs yet</EmptyTitle>
              <EmptyDescription>Submit one with the umm CLI or the REST API.</EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Job</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Progress</TableHead>
                <TableHead>Owner</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data ?? []).map((j) => {
                const p = progress(j)
                return (
                  <TableRow key={j.id} className="cursor-pointer" onClick={() => onOpen(j.id)}>
                    <TableCell>
                      <div className="font-medium">{j.name}</div>
                      <div className="font-mono text-xs text-muted-foreground">{j.id}</div>
                    </TableCell>
                    <TableCell>
                      <StateBadge state={j.state} />
                    </TableCell>
                    <TableCell>
                      <div className="flex min-w-40 items-center gap-2">
                        <Progress value={p.pct} aria-label="job progress" />
                        <span className="text-xs tabular-nums">
                          {p.done}/{p.total}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{j.owner}</TableCell>
                    <TableCell className="text-muted-foreground">{since(j.created_at)}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  )
}

function TaskSheet({ id, onClose }: { id?: string; onClose: () => void }) {
  const [task, setTask] = useState<Task>()
  const [logs, setLogs] = useState<string>()

  // The parent remounts this component (key=id) for each task, so state
  // starts empty for every task.
  useEffect(() => {
    if (!id) return
    api.task(id).then(setTask, (e) => toast.error(String(e)))
    api.logs(id).then(setLogs, () => setLogs(""))
  }, [id])

  return (
    <Sheet open={!!id} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-full sm:max-w-2xl">
        <SheetHeader>
          <SheetTitle>Task {task?.key}</SheetTitle>
          <SheetDescription>
            {task ? (
              <>
                <code>{task.spec.runtime}</code> {task.spec.args?.join(" ")}
              </>
            ) : (
              "Loading…"
            )}
          </SheetDescription>
        </SheetHeader>
        {!task ? (
          <div className="flex justify-center p-6">
            <Spinner />
          </div>
        ) : (
          <Tabs defaultValue="attempts" className="px-4">
            <TabsList>
              <TabsTrigger value="attempts">Attempts</TabsTrigger>
              <TabsTrigger value="logs">Logs</TabsTrigger>
              <TabsTrigger value="placement">Placement</TabsTrigger>
            </TabsList>
            <TabsContent value="attempts">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>#</TableHead>
                    <TableHead>Mac</TableHead>
                    <TableHead>Outcome</TableHead>
                    <TableHead>Score</TableHead>
                    <TableHead>Detail</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(task.attempts ?? []).map((a) => (
                    <TableRow key={a.attempt}>
                      <TableCell>{a.attempt}</TableCell>
                      <TableCell>{a.agent_name || a.agent_id}</TableCell>
                      <TableCell>
                        <StateBadge state={a.outcome} />
                      </TableCell>
                      <TableCell className="tabular-nums">{a.score.toFixed(1)}</TableCell>
                      <TableCell className="max-w-56 truncate text-muted-foreground">{a.error}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {task.error && (
                <Alert className="mt-4">
                  <AlertDescription>{task.error}</AlertDescription>
                </Alert>
              )}
            </TabsContent>
            <TabsContent value="logs">
              <ScrollArea className="h-[60vh] rounded-md border">
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
                      <TableCell className="text-muted-foreground">
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
        )}
      </SheetContent>
    </Sheet>
  )
}

function JobDetail({ id, onBack, canCancel }: { id: string; onBack: () => void; canCancel: boolean }) {
  const { data: job, error, refresh } = usePolling(() => api.job(id), 3000)
  const [taskId, setTaskId] = useState<string>()

  async function cancel() {
    if (!confirm("Cancel this job? Running tasks are stopped.")) return
    try {
      await api.cancelJob(id)
      toast.success("Job cancelled")
      void refresh()
    } catch (e) {
      toast.error(String(e))
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Button variant="ghost" size="icon-sm" onClick={onBack} aria-label="Back to jobs">
            <ArrowLeftIcon />
          </Button>
          {job?.name ?? "Job"} {job && <StateBadge state={job.state} />}
        </CardTitle>
        <CardDescription className="font-mono">{id}</CardDescription>
        <CardAction className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => void refresh()}>
            <RefreshCwIcon data-icon="inline-start" />
            Refresh
          </Button>
          {canCancel && job && !["succeeded", "failed", "cancelled"].includes(job.state) && (
            <Button variant="destructive" size="sm" onClick={cancel}>
              Cancel job
            </Button>
          )}
        </CardAction>
      </CardHeader>
      <CardContent>
        {error && (
          <Alert variant="destructive" className="mb-4">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Task</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Runtime</TableHead>
              <TableHead>Depends on</TableHead>
              <TableHead>Attempt</TableHead>
              <TableHead>Failures</TableHead>
              <TableHead>Mac</TableHead>
              <TableHead>Last error</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(job?.tasks ?? []).map((t) => (
              <TableRow key={t.id} className="cursor-pointer" onClick={() => setTaskId(t.id)}>
                <TableCell className="font-medium">{t.key}</TableCell>
                <TableCell>
                  <StateBadge state={t.state} />
                </TableCell>
                <TableCell>
                  <code className="text-xs">{t.spec.runtime}</code>
                </TableCell>
                <TableCell className="text-muted-foreground">{t.spec.depends_on?.join(", ") || "–"}</TableCell>
                <TableCell className="tabular-nums">{t.attempt}</TableCell>
                <TableCell className="tabular-nums">{t.failures}</TableCell>
                <TableCell className="font-mono text-xs">{t.agent_id || "–"}</TableCell>
                <TableCell className="max-w-64 truncate text-muted-foreground">{t.error || "–"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
      <TaskSheet key={taskId ?? "none"} id={taskId} onClose={() => setTaskId(undefined)} />
    </Card>
  )
}

export function JobsPage({ canCancel }: { canCancel: boolean }) {
  const [open, setOpen] = useState<string>()
  return open ? (
    <JobDetail id={open} onBack={() => setOpen(undefined)} canCancel={canCancel} />
  ) : (
    <JobsList onOpen={setOpen} />
  )
}
