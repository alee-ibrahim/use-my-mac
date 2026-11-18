import { useCallback, useState } from "react"
import { toast } from "sonner"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"

export type ConfirmOptions = {
  title: string
  description: React.ReactNode
  action?: string
  destructive?: boolean
  // run performs the action; a returned string is shown as the success toast.
  run: () => Promise<string | void>
}

// useConfirm returns an AlertDialog element and a function that opens it.
// The dialog stays open (with the button disabled) while the action runs, so
// it also works from dropdown menu items, which unmount on select.
export function useConfirm() {
  const [opts, setOpts] = useState<ConfirmOptions>()
  const [busy, setBusy] = useState(false)

  const ask = useCallback((o: ConfirmOptions) => setOpts(o), [])

  async function confirm(e: React.MouseEvent) {
    e.preventDefault()
    if (!opts) return
    setBusy(true)
    try {
      const msg = await opts.run()
      if (msg) toast.success(msg)
      setOpts(undefined)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const dialog = (
    <AlertDialog open={!!opts} onOpenChange={(o) => !o && !busy && setOpts(undefined)}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{opts?.title}</AlertDialogTitle>
          <AlertDialogDescription>{opts?.description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant={opts?.destructive === false ? "default" : "destructive"}
            disabled={busy}
            onClick={confirm}
          >
            {busy ? "Working…" : (opts?.action ?? "Delete")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
  return [dialog, ask] as const
}
