import { useState } from "react"
import { toast } from "sonner"
import { PlusIcon, Trash2Icon } from "lucide-react"

import { useConfirm } from "@/components/confirm"
import { PageHeader } from "@/components/ops"

import { StateBadge } from "@/components/state-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Skeleton } from "@/components/ui/skeleton"
import { useIsMobile } from "@/hooks/use-mobile"
import { api, since, type APIKey } from "@/lib/api"
import { usePolling } from "@/lib/hooks"

function CreateKey({ onCreated }: { onCreated: () => void }) {
  const [name, setName] = useState("")
  const [role, setRole] = useState("app")
  const [secret, setSecret] = useState<string>()

  async function create(e: React.FormEvent) {
    e.preventDefault()
    try {
      const k = await api.createKey(name, role)
      setSecret(k.secret)
      onCreated()
    } catch (err) {
      toast.error(String(err))
    }
  }

  return (
    <Dialog
      onOpenChange={(o) => {
        if (!o) {
          setSecret(undefined)
          setName("")
        }
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm" className="max-md:h-10">
          <PlusIcon data-icon="inline-start" />
          New key
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New API key</DialogTitle>
          <DialogDescription>
            App keys submit and read their own jobs. Viewer keys see everything read-only. Admin keys manage the
            fleet.
          </DialogDescription>
        </DialogHeader>
        {secret ? (
          <Alert>
            <AlertTitle>Copy it now: it is shown only once</AlertTitle>
            <AlertDescription>
              <code className="break-all">{secret}</code>
            </AlertDescription>
          </Alert>
        ) : (
          <form id="create-key" onSubmit={create}>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="key-name">Name</FieldLabel>
                <Input id="key-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="video-pipeline" required />
              </Field>
              <Field>
                <FieldLabel htmlFor="key-role">Role</FieldLabel>
                <Select value={role} onValueChange={setRole}>
                  <SelectTrigger id="key-role">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="app">app</SelectItem>
                    <SelectItem value="viewer">viewer</SelectItem>
                    <SelectItem value="admin">admin</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
            </FieldGroup>
          </form>
        )}
        <DialogFooter>
          {secret ? (
            <Button variant="outline" onClick={() => navigator.clipboard.writeText(secret).then(() => toast.success("Copied"))}>
              Copy key
            </Button>
          ) : (
            <Button type="submit" form="create-key" disabled={!name}>
              Create
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export function KeysPage() {
  const { data, error, refresh } = usePolling(api.keys, 15000)
  const [confirm, ask] = useConfirm()
  const mobile = useIsMobile()
  const keys = data ?? []
  const revoked = keys.filter((k) => k.revoked_at).length

  function revoke(k: APIKey) {
    ask({
      title: `Revoke ${k.name}?`,
      description: "Apps and people using this key stop working immediately. You can delete it afterwards.",
      action: "Revoke key",
      run: async () => {
        await api.revokeKey(k.id)
        void refresh()
        return "Key revoked"
      },
    })
  }
  function remove(k: APIKey) {
    ask({
      title: `Delete ${k.name}?`,
      description: "The revoked key is removed from the list. The deletion is recorded in the activity log.",
      run: async () => {
        await api.deleteKey(k.id)
        void refresh()
        return "Key deleted"
      },
    })
  }
  function purge() {
    ask({
      title: `Delete ${revoked} revoked key${revoked === 1 ? "" : "s"}?`,
      description: "Active keys are kept. The deletion is recorded in the activity log.",
      run: async () => {
        const r = await api.purgeKeys()
        void refresh()
        return `Deleted ${r.deleted} key${r.deleted === 1 ? "" : "s"}`
      },
    })
  }

  const actions = (k: APIKey) =>
    k.revoked_at ? (
      <Button variant="ghost" size="sm" className="max-md:h-10" onClick={() => remove(k)}>
        <Trash2Icon data-icon="inline-start" />
        Delete
      </Button>
    ) : (
      <Button variant="ghost" size="sm" className="max-md:h-10" onClick={() => revoke(k)}>
        Revoke
      </Button>
    )

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader title="API keys" description="Only hashes are stored. Revoke a key to stop it, then delete it.">
        {revoked > 0 && (
          <Button variant="outline" size="sm" className="max-md:h-10" onClick={purge}>
            <Trash2Icon data-icon="inline-start" />
            Delete {revoked} revoked
          </Button>
        )}
        <CreateKey onCreated={() => void refresh()} />
      </PageHeader>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {!data ? (
        <Skeleton className="h-48 w-full rounded-xl" />
      ) : mobile ? (
        <ul className="flex flex-col gap-2">
          {keys.map((k) => (
            <li key={k.id}>
              <Card size="sm">
                <CardContent className="flex items-center gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="truncate font-medium">{k.name}</span>
                      <Badge variant="outline">{k.role}</Badge>
                      {k.revoked_at && <StateBadge state="revoked" />}
                    </div>
                    <div className="truncate text-xs text-muted-foreground">
                      <span className="font-mono">{k.prefix}…</span> · created {since(k.created_at)} · used{" "}
                      {since(k.last_used_at)}
                    </div>
                  </div>
                  {actions(k)}
                </CardContent>
              </Card>
            </li>
          ))}
        </ul>
      ) : (
        <Card size="sm">
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Prefix</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Last used</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((k) => (
                  <TableRow key={k.id}>
                    <TableCell className="font-medium">{k.name}</TableCell>
                    <TableCell>
                      <Badge variant="outline">{k.role}</Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{k.prefix}…</TableCell>
                    <TableCell className="text-muted-foreground">{since(k.created_at)}</TableCell>
                    <TableCell className="text-muted-foreground">{since(k.last_used_at)}</TableCell>
                    <TableCell>
                      <StateBadge state={k.revoked_at ? "revoked" : "active"} />
                    </TableCell>
                    <TableCell className="text-right">{actions(k)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
      {confirm}
    </div>
  )
}
