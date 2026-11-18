import { useState } from "react"
import { toast } from "sonner"
import { LockIcon, MoreHorizontalIcon, PlusIcon } from "lucide-react"

import { Alert, AlertDescription } from "@/components/ui/alert"
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
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import {
  api,
  since,
  type AccessEvent,
  type AccessPolicy,
  type AccessRule,
  type AccessUser,
  type Me,
} from "@/lib/api"
import { usePolling } from "@/lib/hooks"

async function loadAccess(): Promise<{ policy: AccessPolicy; users: AccessUser[]; events: AccessEvent[] }> {
  const [policy, users, events] = await Promise.all([api.access(), api.accessUsers(), api.accessAudit()])
  return { policy, users, events }
}

function RoleBadge({ role }: { role: string }) {
  if (!role) return <Badge variant="destructive">denied</Badge>
  return <Badge variant={role === "admin" ? "default" : "secondary"}>{role}</Badge>
}

function Locked() {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline">
          <LockIcon data-icon="inline-start" />
          from config
        </Badge>
      </TooltipTrigger>
      <TooltipContent>Set in the router config (Helm values). Change it there.</TooltipContent>
    </Tooltip>
  )
}

type RuleDraft = { kind: string; value: string; role: string; name: string }

// RuleDialog adds a rule, or renames a group (value fixed).
function RuleDialog({
  open,
  onOpenChange,
  initial,
  rename,
  onSaved,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  initial: RuleDraft
  rename?: boolean
  onSaved: () => void
}) {
  const [d, setD] = useState<RuleDraft>(initial)
  const [saving, setSaving] = useState(false)

  async function save(e: React.FormEvent) {
    e.preventDefault()
    setSaving(true)
    try {
      if (rename) {
        await api.setGroupName(d.value, d.name)
      } else {
        await api.putAccessRule({ kind: d.kind, value: d.value.trim(), role: d.role, name: d.name || undefined })
      }
      toast.success(rename ? "Name saved" : "Rule saved")
      onOpenChange(false)
      onSaved()
    } catch (err) {
      toast.error(String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{rename ? "Group name" : "Add rule"}</DialogTitle>
          <DialogDescription>
            {rename
              ? "A display name for this group ID. Only shown here."
              : "A user rule beats group rules; among groups the highest role wins. Rules from the config cannot be lowered."}
          </DialogDescription>
        </DialogHeader>
        <form id="rule-form" onSubmit={save}>
          <FieldGroup>
            {!rename && (
              <div className="grid gap-4 sm:grid-cols-2">
                <Field>
                  <FieldLabel htmlFor="rule-kind">Applies to</FieldLabel>
                  <Select value={d.kind} onValueChange={(v) => setD({ ...d, kind: v })}>
                    <SelectTrigger id="rule-kind">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="group">Group</SelectItem>
                      <SelectItem value="user">User</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
                <Field>
                  <FieldLabel htmlFor="rule-role">Role</FieldLabel>
                  <Select value={d.role} onValueChange={(v) => setD({ ...d, role: v })}>
                    <SelectTrigger id="rule-role">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="viewer">viewer</SelectItem>
                      <SelectItem value="admin">admin</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
              </div>
            )}
            <Field>
              <FieldLabel htmlFor="rule-value">{d.kind === "group" ? "Group ID" : "User"}</FieldLabel>
              <Input
                id="rule-value"
                required
                disabled={rename}
                className="font-mono"
                value={d.value}
                onChange={(e) => setD({ ...d, value: e.target.value })}
                placeholder={d.kind === "group" ? "00000000-0000-0000-0000-000000000000" : "someone@example.com"}
              />
              <FieldDescription>
                {d.kind === "group"
                  ? "The group's object ID (or app-role value) as the identity provider sends it."
                  : "Subject, object ID, email or UPN. Easier: pick the user in Users below."}
              </FieldDescription>
            </Field>
            {d.kind === "group" && (
              <Field>
                <FieldLabel htmlFor="rule-name">Display name (optional)</FieldLabel>
                <Input id="rule-name" value={d.name} onChange={(e) => setD({ ...d, name: e.target.value })} />
              </Field>
            )}
          </FieldGroup>
        </form>
        <DialogFooter>
          <Button type="submit" form="rule-form" disabled={saving || !d.value.trim()}>
            {saving && <Spinner data-icon="inline-start" />}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const emptyDraft: RuleDraft = { kind: "group", value: "", role: "viewer", name: "" }

function ruleLabel(r: AccessRule) {
  return (
    <div className="flex flex-col">
      {r.name && <span className="font-medium">{r.name}</span>}
      <span className="font-mono text-xs break-all text-muted-foreground">{r.value}</span>
    </div>
  )
}

export function AccessPanel({ me }: { me: Me }) {
  const { data, error, refresh: load } = usePolling(loadAccess, 30000)
  const [dialog, setDialog] = useState<{ draft: RuleDraft; rename?: boolean; n: number }>()
  const policy = data?.policy
  const users = data?.users
  const events = data?.events

  async function run(what: () => Promise<unknown>, ok: string) {
    try {
      await what()
      toast.success(ok)
    } catch (err) {
      toast.error(String(err))
    }
    await load()
  }

  function removeRule(r: AccessRule) {
    if (!confirm(`Remove the ${r.kind} rule for ${r.name || r.value}? Sessions are re-evaluated on their next request.`)) return
    void run(() => api.deleteAccessRule(r.id), "Rule removed")
  }

  const userRule = (u: AccessUser) =>
    policy?.rules.find((r) => r.source === "ui" && r.kind === "user" && r.value === u.subject.toLowerCase())

  if (error && !policy) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{error}</AlertDescription>
      </Alert>
    )
  }
  if (!policy || !users || !events) return <Spinner />

  const configViewer = policy.config_default_role === "viewer"

  return (
    <div className="flex flex-col gap-4">
      {!policy.oidc && (
        <Alert>
          <AlertDescription>
            Single sign-on is not configured, so these rules have no effect yet. API keys carry their own role.
          </AlertDescription>
        </Alert>
      )}
      <Card>
        <CardHeader>
          <CardTitle>Everyone else</CardTitle>
          <CardDescription>
            The role for signed-in users no rule matches. To limit who can sign in at all, require assignment on the
            identity provider's app (Microsoft Entra: Enterprise application, "Assignment required"). Changes apply on
            each user's next request.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Field orientation="horizontal" className="max-w-md">
            <FieldLabel htmlFor="default-role">Default role</FieldLabel>
            <Select
              value={configViewer ? "viewer" : policy.ui_default_role}
              disabled={configViewer}
              onValueChange={(v) => void run(() => api.setAccessDefault(v), "Default role saved")}
            >
              <SelectTrigger id="default-role" className="w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="deny">deny</SelectItem>
                <SelectItem value="viewer">viewer</SelectItem>
              </SelectContent>
            </Select>
            {configViewer && <Locked />}
          </Field>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Rules</CardTitle>
          <CardDescription>
            A user rule beats group rules; among groups the highest role wins; then the default. Rules from the config
            are locked and are a floor: nothing here can lower them.
          </CardDescription>
          <CardAction>
            <Button size="sm" onClick={() => setDialog({ draft: emptyDraft, n: Date.now() })}>
              <PlusIcon data-icon="inline-start" />
              Add rule
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Applies to</TableHead>
                <TableHead>Group / user</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Source</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {policy.rules.length === 0 && (
                <TableRow>
                  <TableCell colSpan={5} className="text-muted-foreground">
                    No rules.
                  </TableCell>
                </TableRow>
              )}
              {policy.rules.map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="capitalize">{r.kind}</TableCell>
                  <TableCell>{ruleLabel(r)}</TableCell>
                  <TableCell>
                    {r.locked ? (
                      <RoleBadge role={r.role} />
                    ) : (
                      <Select
                        value={r.role}
                        onValueChange={(v) =>
                          void run(() => api.putAccessRule({ kind: r.kind, value: r.value, role: v }), "Rule saved")
                        }
                      >
                        <SelectTrigger size="sm" className="w-28">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="viewer">viewer</SelectItem>
                          <SelectItem value="admin">admin</SelectItem>
                        </SelectContent>
                      </Select>
                    )}
                  </TableCell>
                  <TableCell>
                    {r.locked ? (
                      <Locked />
                    ) : (
                      <span className="text-xs text-muted-foreground">
                        dashboard{r.created_by ? ` · ${r.created_by}` : ""}
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon" aria-label="Actions">
                          <MoreHorizontalIcon />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuGroup>
                          {r.kind === "group" && (
                            <DropdownMenuItem
                              onSelect={() =>
                                setDialog({
                                  draft: { kind: "group", value: r.value, role: r.role, name: r.name ?? "" },
                                  rename: true,
                                  n: Date.now(),
                                })
                              }
                            >
                              Set display name
                            </DropdownMenuItem>
                          )}
                          {!r.locked && (
                            <DropdownMenuItem variant="destructive" onSelect={() => removeRule(r)}>
                              Remove rule
                            </DropdownMenuItem>
                          )}
                        </DropdownMenuGroup>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Users</CardTitle>
          <CardDescription>
            Everyone who signed in with single sign-on, including people who were turned away. Groups are as of their
            last sign-in. Set a role to add a user rule; "From rules" removes it.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>User</TableHead>
                <TableHead>Last sign-in</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Why</TableHead>
                <TableHead>Set role</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.length === 0 && (
                <TableRow>
                  <TableCell colSpan={5} className="text-muted-foreground">
                    Nobody has signed in with single sign-on yet.
                  </TableCell>
                </TableRow>
              )}
              {users.map((u) => {
                const own = userRule(u)
                const self = me.subject === `user:${u.subject}`
                return (
                  <TableRow key={u.subject}>
                    <TableCell>
                      <div className="flex flex-col">
                        <span className="font-medium">
                          {u.name}
                          {self && <span className="text-muted-foreground"> (you)</span>}
                        </span>
                        <span className="text-xs text-muted-foreground">{u.email || u.upn || u.subject}</span>
                      </div>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{since(u.last_login)}</TableCell>
                    <TableCell>
                      <RoleBadge role={u.role} />
                    </TableCell>
                    <TableCell className="max-w-64 text-xs text-muted-foreground">
                      <span className="flex flex-wrap items-center gap-1">
                        {u.source === "config" && <LockIcon className="size-3" />}
                        {describeReason(u, policy)}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Select
                        value={own?.role ?? "none"}
                        onValueChange={(v) =>
                          void run(() => api.setUserRole(u.subject, v === "none" ? "" : v), "Role saved")
                        }
                      >
                        <SelectTrigger size="sm" className="w-32">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="none">From rules</SelectItem>
                          <SelectItem value="viewer">viewer</SelectItem>
                          <SelectItem value="admin">admin</SelectItem>
                        </SelectContent>
                      </Select>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Audit log</CardTitle>
          <CardDescription>Every role change: who, when and what.</CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>When</TableHead>
                <TableHead>Who</TableHead>
                <TableHead>Change</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {events.length === 0 && (
                <TableRow>
                  <TableCell colSpan={3} className="text-muted-foreground">
                    No changes yet.
                  </TableCell>
                </TableRow>
              )}
              {events.map((e, i) => (
                <TableRow key={i}>
                  <TableCell className="text-muted-foreground" title={new Date(e.at).toLocaleString()}>
                    {since(e.at)}
                  </TableCell>
                  <TableCell className="text-xs">{e.actor}</TableCell>
                  <TableCell className="text-xs">
                    <Badge variant="outline">{e.action}</Badge> <span className="font-mono">{e.detail}</span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {dialog && (
        <RuleDialog
          key={dialog.n}
          open
          onOpenChange={(o) => !o && setDialog(undefined)}
          initial={dialog.draft}
          rename={dialog.rename}
          onSaved={() => void load()}
        />
      )}
    </div>
  )
}

// describeReason names the deciding rule with its display name when known.
function describeReason(u: AccessUser, p: AccessPolicy) {
  const r = p.rules.find((x) => x.id === u.rule_id)
  if (!r) return u.reason
  const what = r.kind === "group" ? `group ${r.name || r.value}` : "user rule"
  return r.locked ? `config ${what}` : `dashboard ${what}`
}
