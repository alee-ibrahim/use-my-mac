import { useEffect, useState } from "react"
import { toast } from "sonner"
import {
  BatteryIcon,
  LaptopIcon,
  LockIcon,
  MoreHorizontalIcon,
  PlugIcon,
} from "lucide-react"

import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Progress } from "@/components/ui/progress"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { api, bytes, duration, since, type Mac, type UpdateEvent } from "@/lib/api"
import { usePolling } from "@/lib/hooks"
import { AddMac, InstallLinks } from "@/pages/install"

function Meter({ value, label }: { value: number; label: string }) {
  if (value < 0) return <span className="text-muted-foreground">–</span>
  return (
    <div className="flex min-w-24 items-center gap-2">
      <Progress value={Math.min(100, value)} aria-label={label} />
      <span className="w-9 text-right text-xs tabular-nums">{Math.round(value)}%</span>
    </div>
  )
}

function Presence({ m }: { m: Mac }) {
  const t = m.telemetry
  if (!t) return <span className="text-muted-foreground">–</span>
  if (!t.console_user) return <span className="text-muted-foreground">nobody</span>
  return (
    <span className="flex items-center gap-1">
      {t.console_user}
      {t.screen_locked ? (
        <LockIcon className="size-3.5 text-muted-foreground" aria-label="screen locked" />
      ) : (
        <span className="text-xs text-muted-foreground">idle {duration(t.user_idle_seconds)}</span>
      )}
    </span>
  )
}

// Version shows the agent version, the version the router wants on this Mac
// and the self-updater's latest state. Clicking it opens the update history.
function Version({ m, onHistory }: { m: Mac; onHistory: () => void }) {
  const u = m.update
  const pending = u?.desired && u.desired !== m.version
  const showState = u?.state && u.state !== "up_to_date"
  return (
    <div className="flex flex-col items-start gap-1">
      <Button variant="link" size="sm" className="h-auto p-0 font-mono text-xs" onClick={onHistory}>
        {m.version || "–"}
        {pending && <span className="text-muted-foreground"> → {u?.desired}</span>}
      </Button>
      {showState && (
        <Tooltip>
          <TooltipTrigger asChild>
            <span>
              <StateBadge state={u.state!} />
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-sm">
            {u.message || u.state} {u.at ? `(${since(u.at)})` : ""}
          </TooltipContent>
        </Tooltip>
      )}
      {!u?.checked_at && m.version && (
        <span className="text-xs text-muted-foreground">no updater</span>
      )}
    </div>
  )
}

function UpdateHistory({ mac, onClose }: { mac?: Mac; onClose: () => void }) {
  const [events, setEvents] = useState<UpdateEvent[]>()
  const [error, setError] = useState<string>()
  const id = mac?.id
  useEffect(() => {
    if (!id) return
    let live = true
    api.macUpdates(id).then(
      (e) => live && setEvents(e),
      (e) => live && setError(String(e)),
    )
    return () => {
      live = false
      setEvents(undefined)
      setError(undefined)
    }
  }, [id])
  return (
    <Dialog open={!!mac} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Agent updates: {mac?.name}</DialogTitle>
          <DialogDescription>
            Running {mac?.version || "unknown"}
            {mac?.update?.desired ? `, router wants ${mac.update.desired}` : ""}
            {mac?.update?.checked_at ? `. Updater last checked ${since(mac.update.checked_at)}.` : ". The updater has not checked in."}
          </DialogDescription>
        </DialogHeader>
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {!events && !error ? (
          <Spinner />
        ) : events && events.length === 0 ? (
          <p className="text-sm text-muted-foreground">No update attempts yet.</p>
        ) : (
          <ScrollArea className="max-h-96">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {events?.map((e, i) => (
                  <TableRow key={i}>
                    <TableCell className="whitespace-nowrap text-xs">{new Date(e.at).toLocaleString()}</TableCell>
                    <TableCell>
                      <StateBadge state={e.state} />
                    </TableCell>
                    <TableCell className="whitespace-nowrap font-mono text-xs">
                      {e.from_version}
                      {e.to_version && e.to_version !== e.from_version ? ` → ${e.to_version}` : ""}
                    </TableCell>
                    <TableCell className="text-xs whitespace-normal">{e.message}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </ScrollArea>
        )}
      </DialogContent>
    </Dialog>
  )
}

export function MacsPage({ admin }: { admin: boolean }) {
  const [links, setLinks] = useState(0)
  return (
    <div className="flex flex-col gap-4">
      <MacsCard admin={admin} onLinkCreated={() => setLinks((n) => n + 1)} />
      {admin && <InstallLinks version={links} />}
    </div>
  )
}

function MacsCard({ admin, onLinkCreated }: { admin: boolean; onLinkCreated: () => void }) {
  const { data, error, refresh } = usePolling(api.macs, 5000)
  const [history, setHistory] = useState<Mac>()

  async function act(fn: () => Promise<void>, ok: string) {
    try {
      await fn()
      toast.success(ok)
      void refresh()
    } catch (e) {
      toast.error(String(e))
    }
  }

  const macs = data ?? []
  const online = macs.filter((m) => m.online).length
  const ready = macs.filter((m) => m.online && m.agent_state === "ready" && !m.drain && !m.opted_out).length

  return (
    <Card>
      <CardHeader>
        <CardTitle>Macs</CardTitle>
        <CardDescription>
          {online} of {macs.length} online, {ready} accepting work. Telemetry refreshes every 5 seconds.
        </CardDescription>
        {admin && (
          <CardAction>
            <AddMac onCreated={onLinkCreated} />
          </CardAction>
        )}
      </CardHeader>
      <CardContent>
        {error && (
          <Alert variant="destructive" className="mb-4">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {data && macs.length === 0 ? (
          <Empty>
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <LaptopIcon />
              </EmptyMedia>
              <EmptyTitle>No Macs yet</EmptyTitle>
              <EmptyDescription>Add a Mac to start building your compute pool.</EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Mac</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>User</TableHead>
                <TableHead>User CPU</TableHead>
                <TableHead>Our CPU</TableHead>
                <TableHead>GPU</TableHead>
                <TableHead>Free memory</TableHead>
                <TableHead>Power</TableHead>
                <TableHead>Thermal</TableHead>
                <TableHead>Tasks</TableHead>
                {admin && <TableHead className="w-10" />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {macs.map((m) => {
                const t = m.telemetry
                return (
                  <TableRow key={m.id}>
                    <TableCell>
                      <div className="font-medium">{m.name}</div>
                      <div className="text-xs text-muted-foreground">
                        {[t?.chip, t?.mem_total_bytes ? bytes(t.mem_total_bytes) : "", `seen ${since(m.last_seen)}`]
                          .filter(Boolean)
                          .join(" · ")}
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        <StateBadge state={m.agent_state} />
                        {m.drain && <Badge variant="outline">drained</Badge>}
                        {m.opted_out && <Badge variant="outline">opted out</Badge>}
                        {m.quarantined_until && (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <Badge variant="destructive">quarantined</Badge>
                            </TooltipTrigger>
                            <TooltipContent>Repeated failures; until {new Date(m.quarantined_until).toLocaleTimeString()}</TooltipContent>
                          </Tooltip>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Version m={m} onHistory={() => setHistory(m)} />
                    </TableCell>
                    <TableCell>
                      <Presence m={m} />
                    </TableCell>
                    <TableCell>
                      <Meter value={t?.user_cpu_percent ?? -1} label="user CPU" />
                    </TableCell>
                    <TableCell>
                      <Meter value={t?.our_cpu_percent ?? -1} label="our CPU" />
                    </TableCell>
                    <TableCell>
                      <Meter value={t?.gpu_util_percent ?? -1} label="GPU" />
                    </TableCell>
                    <TableCell className="tabular-nums">
                      {bytes(t?.mem_available_bytes)}
                      {t?.memory_pressure && t.memory_pressure !== "normal" && (
                        <Badge variant="destructive" className="ml-1">
                          {t.memory_pressure}
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell>
                      {t ? (
                        t.on_ac ? (
                          <PlugIcon className="size-4" aria-label="AC power" />
                        ) : (
                          <span className="flex items-center gap-1">
                            <BatteryIcon className="size-4" aria-label="battery" />
                            {t.battery_percent >= 0 ? `${t.battery_percent}%` : ""}
                          </span>
                        )
                      ) : (
                        "–"
                      )}
                    </TableCell>
                    <TableCell>{t?.thermal || "–"}</TableCell>
                    <TableCell className="tabular-nums">
                      {m.running.length}/{m.max_tasks}
                    </TableCell>
                    {admin && (
                      <TableCell>
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${m.name}`}>
                              <MoreHorizontalIcon />
                            </Button>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end">
                            <DropdownMenuGroup>
                              <DropdownMenuItem
                                onSelect={() =>
                                  act(() => api.patchMac(m.id, { drain: !m.drain }), m.drain ? "Undrained" : "Drained: running tasks finish, no new work")
                                }
                              >
                                {m.drain ? "Undrain" : "Drain"}
                              </DropdownMenuItem>
                              <DropdownMenuItem
                                onSelect={() =>
                                  act(() => api.patchMac(m.id, { opted_out: !m.opted_out }), m.opted_out ? "Opted back in" : "Opted out: running tasks moved elsewhere")
                                }
                              >
                                {m.opted_out ? "Opt back in" : "Opt out now"}
                              </DropdownMenuItem>
                            </DropdownMenuGroup>
                            <DropdownMenuSeparator />
                            <DropdownMenuItem
                              variant="destructive"
                              onSelect={() => {
                                if (confirm(`Remove ${m.name}? Its credential stops working.`))
                                  void act(() => api.deleteMac(m.id), "Removed")
                              }}
                            >
                              Remove
                            </DropdownMenuItem>
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </TableCell>
                    )}
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
        <UpdateHistory mac={history} onClose={() => setHistory(undefined)} />
      </CardContent>
    </Card>
  )
}
