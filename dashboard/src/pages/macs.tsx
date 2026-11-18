import { useState } from "react"
import { toast } from "sonner"
import {
  BatteryIcon,
  LaptopIcon,
  LockIcon,
  MoreHorizontalIcon,
  PlugIcon,
  PlusIcon,
} from "lucide-react"

import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
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
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { api, bytes, duration, since, type Mac } from "@/lib/api"
import { usePolling } from "@/lib/hooks"

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

function AddMac() {
  const [token, setToken] = useState<string>()
  const [busy, setBusy] = useState(false)
  async function mint() {
    setBusy(true)
    try {
      setToken((await api.enrolToken(24)).token)
    } catch (e) {
      toast.error(String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog onOpenChange={(o) => !o && setToken(undefined)}>
      <DialogTrigger asChild>
        <Button size="sm">
          <PlusIcon data-icon="inline-start" />
          Add a Mac
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a Mac</DialogTitle>
          <DialogDescription>
            Install the agent on the Mac, then enrol it with a one-time token. The agent runs as a dedicated
            non-admin user and only connects outbound.
          </DialogDescription>
        </DialogHeader>
        <Textarea
          readOnly
          className="font-mono text-xs"
          rows={4}
          value={`# on the Mac, inside the unpacked umm-agent-darwin-arm64 folder\n# (build it with: make dist-mac)\nsudo UMM_SERVER=${location.origin} sh install.sh\n# paste the token below when asked`}
        />
        {token ? (
          <Alert>
            <AlertTitle>Enrolment token (valid 24h, single use)</AlertTitle>
            <AlertDescription>
              <code className="break-all">{token}</code>
            </AlertDescription>
          </Alert>
        ) : null}
        <DialogFooter>
          {token ? (
            <Button variant="outline" onClick={() => navigator.clipboard.writeText(token).then(() => toast.success("Copied"))}>
              Copy token
            </Button>
          ) : (
            <Button onClick={mint} disabled={busy}>
              {busy && <Spinner data-icon="inline-start" />}
              Create enrolment token
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function MacsPage({ admin }: { admin: boolean }) {
  const { data, error, refresh } = usePolling(api.macs, 5000)

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
            <AddMac />
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
      </CardContent>
    </Card>
  )
}
