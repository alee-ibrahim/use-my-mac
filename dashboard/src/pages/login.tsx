import { useEffect, useState } from "react"
import { KeyRoundIcon, LogInIcon } from "lucide-react"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldSeparator } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { api, type Me } from "@/lib/api"

export function Login({ onLogin }: { onLogin: (me: Me) => void }) {
  const [oidc, setOidc] = useState(false)
  const [key, setKey] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  useEffect(() => {
    api.authConfig().then((c) => setOidc(c.oidc), () => setOidc(false))
  }, [])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      onLogin(await api.loginWithKey(key))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-svh items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>use-my-mac</CardTitle>
          <CardDescription>Sign in to manage your Mac compute pool.</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit}>
            <FieldGroup>
              {oidc && (
                <>
                  <Field>
                    <Button type="button" asChild>
                      <a href={`/auth/login?return=${encodeURIComponent(location.pathname)}`}>
                        <LogInIcon data-icon="inline-start" />
                        Sign in with SSO
                      </a>
                    </Button>
                  </Field>
                  <FieldSeparator>or</FieldSeparator>
                </>
              )}
              <Field>
                <FieldLabel htmlFor="key">API key</FieldLabel>
                <Input
                  id="key"
                  type="password"
                  autoComplete="off"
                  placeholder="umm_key_…"
                  value={key}
                  onChange={(e) => setKey(e.target.value)}
                  required
                />
                <FieldDescription>Admin or viewer keys only. App keys are for the API.</FieldDescription>
              </Field>
              {error && (
                <Alert variant="destructive">
                  <AlertDescription>{error}</AlertDescription>
                </Alert>
              )}
              <Field>
                <Button type="submit" variant={oidc ? "outline" : "default"} disabled={busy || !key}>
                  {busy ? <Spinner data-icon="inline-start" /> : <KeyRoundIcon data-icon="inline-start" />}
                  Sign in with key
                </Button>
              </Field>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
