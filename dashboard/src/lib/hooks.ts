import { useCallback, useEffect, useRef, useState } from "react"

// usePolling loads data now and then every `ms` while the tab is visible.
export function usePolling<T>(load: () => Promise<T>, ms: number) {
  const [data, setData] = useState<T | undefined>()
  const [error, setError] = useState<string | undefined>()
  const loadRef = useRef(load)
  useEffect(() => {
    loadRef.current = load
  }, [load])

  const refresh = useCallback(async () => {
    try {
      setData(await loadRef.current())
      setError(undefined)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    void refresh()
    const id = window.setInterval(() => {
      if (document.visibilityState === "visible") void refresh()
    }, ms)
    return () => window.clearInterval(id)
  }, [refresh, ms])

  return { data, error, refresh }
}
