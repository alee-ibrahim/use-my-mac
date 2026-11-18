import { useState } from "react"
import { toast } from "sonner"
import { BatteryIcon, ChevronRightIcon, LaptopIcon, MoreHorizontalIcon, PlugIcon } from "lucide-react"

import { useConfirm } from "@/components/confirm"
import { PageHeader } from "@/components/ops"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { useIsMobile } from "@/hooks/use-mobile"
import { api, bytes, since, type Mac } from "@/lib/api"
import { href, usePolling } from "@/lib/hooks"
import { MacStateBadges, PresenceLine, RuntimeBadges, VersionLine } from "@/components/mac-bits"
import { AddMac, InstallLinks } from "@/pages/install"

function Meter({ value, label }: { value: number; label: string }) {
  if (value < 0) return <span className="text-muted-foreground">–</span>
  return (
    <div className="flex min-w-20 items-center gap-2">
      <Progress value={Math.min(100, value)} aria-label={label} />
      <span className="w-9 text-right text-xs tabular-nums">{Math.round(value)}%</span>
    </div>
  )
}

// Same rule as the installer and the router: 1-64 of A-Z a-z 0-9 space . _ -
const macName = /^[A-Za-z0-9 ._-]{1,64}$/

function RenameMac({ mac, onClose, onRenamed }: { mac?: Mac; onClose: () => void; onRenamed: () => void }) {
  // Keyed by the Mac's ID by the caller, so this starts from its current name.
  const [name, setName] = useState(mac?.name ?? "")
  const [busy, setBusy] = useState(false)
  const trimmed = name.trim()
  const valid = macName.test(trimmed)

  async function save(e: React.FormEvent) {
    e.preventDefault()
    if (!mac || !valid) return
    setBusy(true)
    try {
      await api.patchMac(mac.id, { name: trimmed })
      toast.success(`Renamed to ${trimmed}`)
      onRenamed()
      onClose()
    } catch (err) {
      toast.error(String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={!!mac} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rename {mac?.name}</DialogTitle>
          <DialogDescription>
            Changes the name shown here and in metrics. The Mac keeps its ID, credential and work; its own
            agent.yaml is not changed.
          </DialogDescription>
        </DialogHeader>
        <form id="rename-mac" onSubmit={save}>
          <FieldGroup>
            <Field data-invalid={!!trimmed && !valid}>
              <FieldLabel htmlFor="mac-name">Name</FieldLabel>
              <Input
                id="mac-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                maxLength={64}
                aria-invalid={!!trimmed && !valid}
                required
              />
              <FieldDescription>Up to 64 letters, digits, spaces, dots, underscores or hyphens.</FieldDescription>
            </Field>
          </FieldGroup>
        </form>
        <DialogFooter>
          <Button type="submit" form="rename-mac" disabled={!valid || busy || trimmed === mac?.name}>
            {busy && <Spinner data-icon="inline-start" />}
            Rename
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// MacActions: drain, opt out and delete (admin). Delete asks first.
export function MacActions({
  m,
  onChanged,
  onDeleted,
}: {
  m: Mac
  onChanged: () => void
  onDeleted?: () => void
}) {
  const [confirm, ask] = useConfirm()
  const [renaming, setRenaming] = useState<Mac>()
  async function act(fn: () => Promise<void>, ok: string) {
    try {
      await fn()
      toast.success(ok)
      onChanged()
    } catch (e) {
      toast.error(String(e))
    }
  }
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            aria-label={`Actions for ${m.name}`}
            onClick={(e) => e.stopPropagation()}
          >
            <MoreHorizontalIcon />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()}>
          <DropdownMenuLabel>{m.name}</DropdownMenuLabel>
          <DropdownMenuGroup>
            <DropdownMenuItem onSelect={() => setRenaming(m)}>Rename…</DropdownMenuItem>
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuGroup>
            <DropdownMenuItem
              onSelect={() =>
                act(
                  () => api.patchMac(m.id, { drain: !m.drain }),
                  m.drain ? "Undrained" : "Drained: running tasks finish, no new work"
                )
              }
            >
              {m.drain ? "Undrain" : "Drain"}
            </DropdownMenuItem>
            <DropdownMenuItem
              onSelect={() =>
                act(
                  () => api.patchMac(m.id, { opted_out: !m.opted_out }),
                  m.opted_out ? "Opted back in" : "Opted out: running tasks moved elsewhere"
                )
              }
            >
              {m.opted_out ? "Opt back in" : "Opt out now"}
            </DropdownMenuItem>
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            variant="destructive"
            onSelect={() =>
              ask({
                title: `Delete ${m.name}?`,
                description: (
                  <>
                    Its credential stops working at once and its metrics history is removed. To use it again,
                    install it with a new link.
                    {m.online && (
                      <strong className="mt-2 block">
                        This Mac is online{m.running.length ? ` and running ${m.running.length} task(s)` : ""}; its
                        tasks move to other Macs.
                      </strong>
                    )}
                  </>
                ),
                action: "Delete Mac",
                run: async () => {
                  await api.deleteMac(m.id)
                  onDeleted?.()
                  onChanged()
                  return `${m.name} deleted`
                },
              })
            }
          >
            Delete…
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {confirm}
      <RenameMac key={renaming?.id} mac={renaming} onClose={() => setRenaming(undefined)} onRenamed={onChanged} />
    </>
  )
}

export function MacsPage({ admin }: { admin: boolean }) {
  const [links, setLinks] = useState(0)
  const { data, error, refresh } = usePolling(api.macs, 5000)
  const mobile = useIsMobile()
  const macs = data ?? []
  const online = macs.filter((m) => m.online).length
  const ready = macs.filter((m) => m.online && m.agent_state === "ready" && !m.drain && !m.opted_out).length

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="Macs"
        description={
          data ? `${online} of ${macs.length} online, ${ready} accepting work. Refreshes every 5 seconds.` : "Loading…"
        }
      >
        {admin && <AddMac onCreated={() => setLinks((n) => n + 1)} />}
      </PageHeader>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {!data ? (
        <Skeleton className="h-64 w-full rounded-xl" />
      ) : macs.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <LaptopIcon />
            </EmptyMedia>
            <EmptyTitle>No Macs yet</EmptyTitle>
            <EmptyDescription>Add a Mac to start building your compute pool.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : mobile ? (
        <ul className="flex flex-col gap-3">
          {macs.map((m) => (
            <li key={m.id}>
              <MacListCard m={m} admin={admin} onChanged={() => void refresh()} />
            </li>
          ))}
        </ul>
      ) : (
        <Card size="sm">
          <CardContent>
            <MacTable macs={macs} admin={admin} onChanged={() => void refresh()} />
          </CardContent>
        </Card>
      )}
      {admin && <InstallLinks version={links} />}
    </div>
  )
}

function MacListCard({ m, admin, onChanged }: { m: Mac; admin: boolean; onChanged: () => void }) {
  const t = m.online ? m.telemetry : undefined
  return (
    <Card size="sm">
      <CardContent className="flex flex-col gap-2">
        <div className="flex items-start gap-2">
          <a href={href("mac", m.id)} className="flex min-h-11 min-w-0 flex-1 flex-col justify-center">
            <span className="truncate font-medium">{m.name}</span>
            <span className="truncate text-xs text-muted-foreground">
              {[t?.chip, `seen ${since(m.last_seen)}`].filter(Boolean).join(" · ")}
            </span>
          </a>
          {admin ? (
            <MacActions m={m} onChanged={onChanged} />
          ) : (
            <Button variant="ghost" size="icon" className="size-11" asChild>
              <a href={href("mac", m.id)} aria-label={`Open ${m.name}`}>
                <ChevronRightIcon />
              </a>
            </Button>
          )}
        </div>
        <MacStateBadges m={m} />
        <div className="text-sm">
          <PresenceLine m={m} />
        </div>
        <RuntimeBadges m={m} />
        <dl className="grid grid-cols-3 gap-2 text-xs">
          <div>
            <dt className="text-muted-foreground">Our CPU</dt>
            <dd className="tabular-nums">{t ? `${Math.round(t.our_cpu_percent)}%` : "–"}</dd>
          </div>
          <div>
            <dt className="text-muted-foreground">User CPU</dt>
            <dd className="tabular-nums">{t ? `${Math.round(t.user_cpu_percent)}%` : "–"}</dd>
          </div>
          <div>
            <dt className="text-muted-foreground">Tasks</dt>
            <dd className="tabular-nums">
              {m.running.length}/{m.max_tasks}
            </dd>
          </div>
        </dl>
        <VersionLine m={m} />
      </CardContent>
    </Card>
  )
}

function MacTable({ macs, admin, onChanged }: { macs: Mac[]; admin: boolean; onChanged: () => void }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Mac</TableHead>
          <TableHead>State</TableHead>
          <TableHead>User</TableHead>
          <TableHead>User CPU</TableHead>
          <TableHead>Our CPU</TableHead>
          <TableHead className="hidden xl:table-cell">GPU</TableHead>
          <TableHead className="hidden lg:table-cell">Free memory</TableHead>
          <TableHead className="hidden xl:table-cell">Power</TableHead>
          <TableHead className="hidden xl:table-cell">Thermal</TableHead>
          <TableHead>Tasks</TableHead>
          <TableHead className="hidden lg:table-cell">Runtimes</TableHead>
          <TableHead className="hidden lg:table-cell">Version</TableHead>
          {admin && <TableHead className="w-10" />}
        </TableRow>
      </TableHeader>
      <TableBody>
        {macs.map((m) => {
          const t = m.online ? m.telemetry : undefined
          return (
            <TableRow key={m.id} className="cursor-pointer" onClick={() => (location.hash = href("mac", m.id))}>
              <TableCell>
                <a
                  href={href("mac", m.id)}
                  className="font-medium hover:underline"
                  onClick={(e) => e.stopPropagation()}
                >
                  {m.name}
                </a>
                <div className="text-xs text-muted-foreground">
                  {[t?.chip, t?.mem_total_bytes ? bytes(t.mem_total_bytes) : "", `seen ${since(m.last_seen)}`]
                    .filter(Boolean)
                    .join(" · ")}
                </div>
              </TableCell>
              <TableCell>
                <MacStateBadges m={m} />
              </TableCell>
              <TableCell className="max-w-48">
                <PresenceLine m={m} />
              </TableCell>
              <TableCell>
                <Meter value={t?.user_cpu_percent ?? -1} label="user CPU" />
              </TableCell>
              <TableCell>
                <Meter value={t?.our_cpu_percent ?? -1} label="our CPU" />
              </TableCell>
              <TableCell className="hidden xl:table-cell">
                <Meter value={t?.gpu_util_percent ?? -1} label="GPU" />
              </TableCell>
              <TableCell className="hidden tabular-nums lg:table-cell">
                {bytes(t?.mem_available_bytes)}
                {t?.memory_pressure && t.memory_pressure !== "normal" && (
                  <Badge variant="destructive" className="ml-1">
                    {t.memory_pressure}
                  </Badge>
                )}
              </TableCell>
              <TableCell className="hidden xl:table-cell">
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
              <TableCell className="hidden xl:table-cell">{t?.thermal || "–"}</TableCell>
              <TableCell className="tabular-nums">
                {m.running.length}/{m.max_tasks}
              </TableCell>
              <TableCell className="hidden max-w-48 lg:table-cell">
                <RuntimeBadges m={m} />
              </TableCell>
              <TableCell className="hidden lg:table-cell">
                <VersionLine m={m} />
              </TableCell>
              {admin && (
                <TableCell onClick={(e) => e.stopPropagation()}>
                  <MacActions m={m} onChanged={onChanged} />
                </TableCell>
              )}
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
