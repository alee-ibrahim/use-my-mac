import { useState } from "react"
import { toast } from "sonner"
import { PlusIcon } from "lucide-react"

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
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { api, since } from "@/lib/api"
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
        <Button size="sm">
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

  async function revoke(id: string, name: string) {
    if (!confirm(`Revoke ${name}? Apps using it stop working immediately.`)) return
    try {
      await api.revokeKey(id)
      toast.success("Key revoked")
      void refresh()
    } catch (e) {
      toast.error(String(e))
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>API keys</CardTitle>
        <CardDescription>Only hashes are stored. Revoked keys stay listed for audit.</CardDescription>
        <CardAction>
          <CreateKey onCreated={() => void refresh()} />
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
            {(data ?? []).map((k) => (
              <TableRow key={k.id}>
                <TableCell className="font-medium">{k.name}</TableCell>
                <TableCell>
                  <Badge variant="outline">{k.role}</Badge>
                </TableCell>
                <TableCell className="font-mono text-xs">{k.prefix}…</TableCell>
                <TableCell className="text-muted-foreground">{since(k.created_at)}</TableCell>
                <TableCell className="text-muted-foreground">{since(k.last_used_at)}</TableCell>
                <TableCell>{k.revoked_at ? <StateBadge state="cancelled" /> : <StateBadge state="ready" />}</TableCell>
                <TableCell className="text-right">
                  {!k.revoked_at && (
                    <Button variant="ghost" size="sm" onClick={() => revoke(k.id, k.name)}>
                      Revoke
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}
