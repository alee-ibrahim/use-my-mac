import { toast } from "sonner"
import { Trash2Icon } from "lucide-react"

import { useConfirm } from "@/components/confirm"
import { PageHeader } from "@/components/ops"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { useIsMobile } from "@/hooks/use-mobile"
import { api, since, type RuntimeCatalogEntry, type RuntimeMacStatus } from "@/lib/api"
import { usePolling } from "@/lib/hooks"

const stateOrder = ["installed", "installing", "outdated", "installable", "failed"]
const stateVariant: Record<string, "default" | "secondary" | "destructive" | "outline"> = {
  installed: "default",
  installing: "secondary",
  outdated: "outline",
  installable: "outline",
  failed: "destructive",
}

// MacStates shows how many Macs are in each state; hover lists them.
function MacStates({ macs }: { macs?: RuntimeMacStatus[] }) {
  if (!macs?.length) return <span className="text-muted-foreground">no Mac can install it</span>
  const groups = new Map<string, RuntimeMacStatus[]>()
  for (const m of macs) groups.set(m.state, [...(groups.get(m.state) ?? []), m])
  const states = [...groups.keys()].sort((a, b) => stateOrder.indexOf(a) - stateOrder.indexOf(b))
  return (
    <div className="flex flex-wrap gap-1">
      {states.map((s) => (
        <Tooltip key={s}>
          <TooltipTrigger asChild>
            <Badge variant={stateVariant[s] ?? "outline"}>
              {groups.get(s)!.length} {s}
            </Badge>
          </TooltipTrigger>
          <TooltipContent className="max-w-80">
            <ul>
              {groups.get(s)!.map((m) => (
                <li key={m.id}>
                  {m.name || m.id}
                  {m.version && ` (${m.version})`}
                  {m.error && `: ${m.error}`}
                </li>
              ))}
            </ul>
          </TooltipContent>
        </Tooltip>
      ))}
    </div>
  )
}

function platforms(e: RuntimeCatalogEntry) {
  return e.package.artifacts.map((a) => `${a.os}/${a.arch}`).join(", ")
}

export function RuntimesPage({ admin }: { admin: boolean }) {
  const { data, error, refresh } = usePolling(api.runtimes, 15000)
  const [confirm, ask] = useConfirm()
  const mobile = useIsMobile()
  const pkgs = data?.packages ?? []

  async function prewarm(e: RuntimeCatalogEntry, on: boolean) {
    try {
      await api.setRuntimePrewarm(e.package.name, on)
      toast.success(on ? `Macs will pre-install ${e.package.name}` : `${e.package.name} installs on demand only`)
      void refresh()
    } catch (err) {
      toast.error(String(err))
    }
  }
  function remove(e: RuntimeCatalogEntry) {
    ask({
      title: `Remove ${e.package.name}?`,
      description:
        "Macs stop offering its commands and delete it once no running task uses it. Tasks that need it are no longer placed.",
      action: "Remove package",
      run: async () => {
        await api.deleteRuntime(e.package.name)
        void refresh()
        return "Package removed"
      },
    })
  }

  const prewarmSwitch = (e: RuntimeCatalogEntry) => (
    <Switch
      checked={e.prewarm}
      disabled={!admin}
      onCheckedChange={(on) => void prewarm(e, on)}
      aria-label={`Pre-install ${e.package.name} on every Mac`}
    />
  )
  const removeButton = (e: RuntimeCatalogEntry) =>
    admin && (
      <Button variant="ghost" size="sm" className="max-md:h-10" onClick={() => remove(e)}>
        <Trash2Icon data-icon="inline-start" />
        Remove
      </Button>
    )

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="Runtimes"
        description="Signed packages Macs install by themselves when a task needs them. Publish with umm runtimes publish."
      />
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {!data ? (
        <Skeleton className="h-48 w-full rounded-xl" />
      ) : pkgs.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No managed runtimes</EmptyTitle>
            <EmptyDescription>
              Sign a package manifest with <code>umm runtimes sign</code> and publish it with{" "}
              <code>umm runtimes publish</code>. See the docs under Runtimes.
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : mobile ? (
        <ul className="flex flex-col gap-2">
          {pkgs.map((e) => (
            <li key={e.package.name}>
              <Card size="sm">
                <CardContent className="flex flex-col gap-2">
                  <div className="flex items-center gap-2">
                    <span className="truncate font-medium">{e.package.name}</span>
                    <Badge variant="outline">{e.package.version}</Badge>
                    <span className="flex-1" />
                    {removeButton(e)}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    {Object.keys(e.package.commands).sort().join(", ")} · {platforms(e)}
                  </div>
                  <MacStates macs={e.macs} />
                  <label className="flex items-center gap-2 text-sm">
                    {prewarmSwitch(e)} Pre-install on every Mac
                  </label>
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
                  <TableHead>Package</TableHead>
                  <TableHead>Commands</TableHead>
                  <TableHead>Platforms</TableHead>
                  <TableHead>Macs</TableHead>
                  <TableHead>Pre-install</TableHead>
                  <TableHead>Published</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {pkgs.map((e) => (
                  <TableRow key={e.package.name}>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <span className="font-medium">{e.package.name}</span>
                        <Badge variant="outline">{e.package.version}</Badge>
                      </div>
                      {e.package.description && (
                        <div className="max-w-72 truncate text-xs text-muted-foreground">{e.package.description}</div>
                      )}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {Object.keys(e.package.commands).sort().join(", ")}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{platforms(e)}</TableCell>
                    <TableCell>
                      <MacStates macs={e.macs} />
                    </TableCell>
                    <TableCell>{prewarmSwitch(e)}</TableCell>
                    <TableCell className="text-muted-foreground">
                      {since(e.published_at)}
                      {e.published_by && ` by ${e.published_by}`}
                    </TableCell>
                    <TableCell className="text-right">{removeButton(e)}</TableCell>
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
