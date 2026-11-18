import { useEffect, useState } from "react"
import { toast } from "sonner"
import { CopyIcon, MoreHorizontalIcon, PlusIcon, Trash2Icon } from "lucide-react"

import { useConfirm } from "@/components/confirm"
import { useIsMobile } from "@/hooks/use-mobile"

import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
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
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import {
  api,
  since,
  type InstallEvent,
  type InstallProfile,
  type InstallToken,
} from "@/lib/api"
import { usePolling } from "@/lib/hooks"

const expiries = [
  { label: "15 minutes", minutes: 15 },
  { label: "1 hour", minutes: 60 },
  { label: "4 hours", minutes: 240 },
  { label: "24 hours", minutes: 1440 },
  { label: "7 days", minutes: 10080 },
]

function parseLabels(s: string): Record<string, string> | string {
  const out: Record<string, string> = {}
  for (const part of s.split(/[,\n]/)) {
    const p = part.trim()
    if (!p) continue
    const i = p.indexOf("=")
    if (i <= 0) return `"${p}" is not key=value`
    out[p.slice(0, i).trim()] = p.slice(i + 1).trim()
  }
  return out
}

function copy(text: string) {
  navigator.clipboard.writeText(text).then(
    () => toast.success("Copied"),
    () => toast.error("Could not copy; select the text instead")
  )
}

function CopyLine({ value, label }: { value: string; label: string }) {
  return (
    <InputGroup>
      <InputGroupInput
        readOnly
        value={value}
        aria-label={label}
        className="font-mono text-xs"
        onFocus={(e) => e.target.select()}
      />
      <InputGroupAddon align="inline-end">
        <InputGroupButton
          aria-label={`Copy ${label}`}
          onClick={() => copy(value)}
        >
          <CopyIcon />
          Copy
        </InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
  )
}

function OneCommand({ onCreated }: { onCreated: () => void }) {
  const [profiles, setProfiles] = useState<InstallProfile[]>()
  const [profile, setProfile] = useState("default")
  const [labels, setLabels] = useState("")
  const [ttl, setTTL] = useState("60")
  const [single, setSingle] = useState(true)
  const [uses, setUses] = useState("5")
  const [note, setNote] = useState("")
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState<InstallToken>()

  useEffect(() => {
    api.installProfiles().then(
      (ps) => {
        setProfiles(ps)
        if (ps.length && !ps.some((p) => p.name === "default"))
          setProfile(ps[0].name)
      },
      (e) => toast.error(String(e))
    )
  }, [])

  const selected = profiles?.find((p) => p.name === profile)
  const parsed = parseLabels(labels)
  const usesN = Number(uses)
  const usesOK =
    single || (Number.isInteger(usesN) && usesN >= 1 && usesN <= 1000)

  async function create() {
    if (typeof parsed === "string") return
    setBusy(true)
    try {
      const t = await api.createInstallToken({
        profile,
        labels: parsed,
        ttl_minutes: Number(ttl),
        max_uses: single ? 1 : usesN,
        note: note.trim() || undefined,
      })
      setCreated(t)
      onCreated()
    } catch (e) {
      toast.error(String(e))
    } finally {
      setBusy(false)
    }
  }

  if (created?.command) {
    const uninstall = `curl -fsSL ${location.origin}/i/uninstall | sudo sh`
    return (
      <div className="flex flex-col gap-4">
        <Alert>
          <AlertTitle>Run this in Terminal on the Mac</AlertTitle>
          <AlertDescription>
            It asks for the Mac admin's password (sudo), verifies the signed
            agent release, installs the agent and waits until the Mac reports
            in. Valid until {new Date(created.expires_at).toLocaleString()} for{" "}
            {created.max_uses === 1 ? "one Mac" : `${created.max_uses} Macs`}.
            It is shown only once.
          </AlertDescription>
        </Alert>
        <CopyLine value={created.command} label="install command" />
        <Field>
          <FieldLabel>Uninstall later</FieldLabel>
          <CopyLine value={uninstall} label="uninstall command" />
          <FieldDescription>
            Add <code>-s -- --purge</code> after <code>sh</code> to also remove
            config, credential and logs.
          </FieldDescription>
        </Field>
        <DialogFooter>
          <Button variant="outline" onClick={() => setCreated(undefined)}>
            Create another
          </Button>
        </DialogFooter>
      </div>
    )
  }

  return (
    <FieldGroup>
      <Field>
        <FieldLabel htmlFor="ins-profile">Profile</FieldLabel>
        <Select value={profile} onValueChange={setProfile} disabled={!profiles}>
          <SelectTrigger id="ins-profile" className="w-full">
            <SelectValue placeholder="Loading…" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {profiles?.map((p) => (
                <SelectItem key={p.name} value={p.name}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        {selected && (
          <FieldDescription>
            Runtimes: {Object.keys(selected.runtimes).sort().join(", ")}
            {selected.labels && Object.keys(selected.labels).length
              ? `. Labels: ${Object.entries(selected.labels)
                  .map(([k, v]) => `${k}=${v}`)
                  .join(", ")}`
              : ""}
            . Set profiles in the router config (install.profiles).
          </FieldDescription>
        )}
      </Field>
      <Field data-invalid={typeof parsed === "string" || undefined}>
        <FieldLabel htmlFor="ins-labels">Extra labels</FieldLabel>
        <Input
          id="ins-labels"
          placeholder="room=lab-2, floor=3"
          value={labels}
          aria-invalid={typeof parsed === "string" || undefined}
          onChange={(e) => setLabels(e.target.value)}
        />
        <FieldDescription>
          {typeof parsed === "string"
            ? parsed
            : "Added to the profile's labels."}
        </FieldDescription>
      </Field>
      <Field>
        <FieldLabel htmlFor="ins-ttl">Link expires after</FieldLabel>
        <Select value={ttl} onValueChange={setTTL}>
          <SelectTrigger id="ins-ttl" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {expiries.map((x) => (
                <SelectItem key={x.minutes} value={String(x.minutes)}>
                  {x.label}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
      </Field>
      <Field orientation="horizontal">
        <Switch id="ins-single" checked={single} onCheckedChange={setSingle} />
        <FieldLabel htmlFor="ins-single">Single use (one Mac)</FieldLabel>
      </Field>
      {!single && (
        <Field data-invalid={!usesOK || undefined}>
          <FieldLabel htmlFor="ins-uses">Number of Macs</FieldLabel>
          <Input
            id="ins-uses"
            type="number"
            min={1}
            max={1000}
            value={uses}
            aria-invalid={!usesOK || undefined}
            onChange={(e) => setUses(e.target.value)}
          />
          <FieldDescription>
            For a batch: each successful enrolment uses up one. Fetching the
            script does not.
          </FieldDescription>
        </Field>
      )}
      <Field>
        <FieldLabel htmlFor="ins-note">Note</FieldLabel>
        <Input
          id="ins-note"
          placeholder="optional, e.g. lab rollout"
          maxLength={200}
          value={note}
          onChange={(e) => setNote(e.target.value)}
        />
      </Field>
      <DialogFooter>
        <Button
          onClick={create}
          disabled={busy || !profiles || typeof parsed === "string" || !usesOK}
        >
          {busy && <Spinner data-icon="inline-start" />}
          Create command
        </Button>
      </DialogFooter>
    </FieldGroup>
  )
}

function Manual() {
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
    <div className="flex flex-col gap-4">
      <Textarea
        readOnly
        className="font-mono text-xs"
        rows={4}
        value={`# on the Mac, inside the unpacked umm-agent-darwin-arm64 folder\n# (build it with: make dist-mac)\nsudo UMM_SERVER=${location.origin} sh install.sh\n# paste the token below when asked`}
      />
      {token && (
        <Alert>
          <AlertTitle>Enrolment token (valid 24h, single use)</AlertTitle>
          <AlertDescription>
            <code className="break-all">{token}</code>
          </AlertDescription>
        </Alert>
      )}
      <DialogFooter>
        {token ? (
          <Button variant="outline" onClick={() => copy(token)}>
            Copy token
          </Button>
        ) : (
          <Button onClick={mint} disabled={busy}>
            {busy && <Spinner data-icon="inline-start" />}
            Create enrolment token
          </Button>
        )}
      </DialogFooter>
    </div>
  )
}

export function AddMac({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm">
          <PlusIcon data-icon="inline-start" />
          Add Mac
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a Mac</DialogTitle>
          <DialogDescription>
            The agent runs as a hidden, non-admin user, only connects outbound
            and shows nothing to the Mac's user.
          </DialogDescription>
        </DialogHeader>
        {open && (
          <Tabs defaultValue="command">
            <TabsList>
              <TabsTrigger value="command">One command</TabsTrigger>
              <TabsTrigger value="manual">Manual</TabsTrigger>
            </TabsList>
            <TabsContent value="command">
              <OneCommand onCreated={onCreated} />
            </TabsContent>
            <TabsContent value="manual">
              <Manual />
            </TabsContent>
          </Tabs>
        )}
      </DialogContent>
    </Dialog>
  )
}

function AuditLog({
  token,
  onClose,
}: {
  token?: InstallToken
  onClose: () => void
}) {
  const [events, setEvents] = useState<InstallEvent[]>()
  const [error, setError] = useState<string>()
  const id = token?.id
  useEffect(() => {
    if (!id) return
    let live = true
    api.installEvents(id).then(
      (e) => live && setEvents(e),
      (e) => live && setError(String(e))
    )
    return () => {
      live = false
      setEvents(undefined)
      setError(undefined)
    }
  }, [id])
  return (
    <Dialog open={!!token} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Install link {token?.prefix}…</DialogTitle>
          <DialogDescription>
            Script fetches, downloads, enrolments and refusals, with the address
            they came from.
          </DialogDescription>
        </DialogHeader>
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {!events && !error ? (
          <Spinner />
        ) : (
          <ScrollArea className="max-h-96">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead>IP</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {events?.map((e, i) => (
                  <TableRow key={i}>
                    <TableCell className="text-xs whitespace-nowrap">
                      {new Date(e.at).toLocaleString()}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          e.kind === "denied" ? "destructive" : "outline"
                        }
                      >
                        {e.kind}
                      </Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {e.ip || "–"}
                    </TableCell>
                    <TableCell className="text-xs whitespace-normal">
                      {e.detail}
                      {e.agent_id ? ` (${e.agent_id})` : ""}
                    </TableCell>
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

export function InstallLinks({ version }: { version: number }) {
  const { data, error, refresh } = usePolling(api.installTokens, 15000)
  const [log, setLog] = useState<InstallToken>()
  const [confirm, ask] = useConfirm()
  const mobile = useIsMobile()
  useEffect(() => {
    if (version) void refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [version])
  if (!data?.length && !error) return null
  const dead = (data ?? []).filter((t) => t.state !== "active").length

  function revoke(t: InstallToken) {
    ask({
      title: "Revoke this install link?",
      description: `${t.prefix}… stops working at once. Macs already installed with it are not affected.`,
      action: "Revoke link",
      run: async () => {
        await api.revokeInstallToken(t.id)
        void refresh()
        return "Install link revoked"
      },
    })
  }
  function remove(t: InstallToken) {
    ask({
      title: "Delete this install link?",
      description: `${t.prefix}… and its audit log are removed. Macs installed with it are not affected.`,
      run: async () => {
        await api.deleteInstallToken(t.id)
        void refresh()
        return "Install link deleted"
      },
    })
  }
  function purge() {
    ask({
      title: `Delete ${dead} used, expired or revoked link${dead === 1 ? "" : "s"}?`,
      description: "Links that still work are kept. Each deletion is recorded in the activity log.",
      run: async () => {
        const r = await api.purgeInstallTokens()
        void refresh()
        return `Deleted ${r.deleted} link${r.deleted === 1 ? "" : "s"}`
      },
    })
  }

  const menu = (t: InstallToken) => (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" className="max-md:size-11" aria-label="Install link actions">
          <MoreHorizontalIcon />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuGroup>
          <DropdownMenuItem onSelect={() => setLog(t)}>Audit log</DropdownMenuItem>
          {t.state === "active" ? (
            <DropdownMenuItem variant="destructive" onSelect={() => revoke(t)}>
              Revoke…
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem variant="destructive" onSelect={() => remove(t)}>
              Delete…
            </DropdownMenuItem>
          )}
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )

  return (
    <Card size="sm">
      <CardHeader>
        <CardTitle>Install links</CardTitle>
        <CardDescription>One-line install commands. Revoke a link to stop it working at once.</CardDescription>
        {dead > 0 && (
          <CardAction>
            <Button variant="outline" size="sm" className="max-md:h-10" onClick={purge}>
              <Trash2Icon data-icon="inline-start" />
              Clear {dead} old
            </Button>
          </CardAction>
        )}
      </CardHeader>
      <CardContent>
        {error && (
          <Alert variant="destructive" className="mb-4">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {mobile ? (
          <ul className="flex flex-col">
            {data?.map((t) => (
              <li key={t.id} className="flex items-center gap-3 border-b py-2 last:border-b-0">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="font-mono text-xs">{t.prefix}…</span>
                    <StateBadge state={t.state} />
                  </div>
                  <div className="truncate text-xs text-muted-foreground">
                    {t.profile.name} · used {t.uses}/{t.max_uses} · {since(t.created_at)}
                    {t.note ? ` · ${t.note}` : ""}
                  </div>
                </div>
                {menu(t)}
              </li>
            ))}
          </ul>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Link</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Profile</TableHead>
                <TableHead>Used</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="w-10" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {data?.map((t) => (
                <TableRow key={t.id}>
                  <TableCell>
                    <div className="font-mono text-xs">{t.prefix}…</div>
                    {t.note && <div className="text-xs text-muted-foreground">{t.note}</div>}
                  </TableCell>
                  <TableCell>
                    <StateBadge state={t.state} />
                  </TableCell>
                  <TableCell className="text-xs">
                    {t.profile.name}
                    {t.profile.labels && Object.keys(t.profile.labels).length > 0 && (
                      <div className="text-muted-foreground">
                        {Object.entries(t.profile.labels)
                          .map(([k, v]) => `${k}=${v}`)
                          .join(", ")}
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="tabular-nums">
                    {t.uses}/{t.max_uses}
                  </TableCell>
                  <TableCell className="text-xs whitespace-nowrap">{new Date(t.expires_at).toLocaleString()}</TableCell>
                  <TableCell className="text-xs whitespace-nowrap">
                    {since(t.created_at)}
                    {t.created_by && <div className="text-muted-foreground">{t.created_by}</div>}
                  </TableCell>
                  <TableCell>{menu(t)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <AuditLog token={log} onClose={() => setLog(undefined)} />
        {confirm}
      </CardContent>
    </Card>
  )
}
