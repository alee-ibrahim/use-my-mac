import { useState } from "react"
import { BookOpenIcon, CircleAlertIcon, KeyRoundIcon, LogInIcon, RotateCwIcon } from "lucide-react"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldSeparator } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { api, ssoURL, type AuthConfig, type Me } from "@/lib/api"

function AuthCard({ description, children }: { description: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-svh flex-col items-center justify-center gap-2 p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>use-my-mac</CardTitle>
          <CardDescription>{description}</CardDescription>
        </CardHeader>
        {children}
      </Card>
      <Button variant="link" size="sm" asChild>
        <a href="/docs">
          <BookOpenIcon data-icon="inline-start" />
          Documentation
        </a>
      </Button>
    </div>
  )
}

// Where to land after signing in: the current page, unless this is one of
// the sign-in pages themselves.
function returnTo() {
  const p = location.pathname
  if (p === "/login" || p === "/signed-out" || p === "/signin-error") return "/"
  return p + location.search + location.hash
}

export function Login({ cfg, onLogin }: { cfg: AuthConfig; onLogin: (me: Me) => void }) {
  const [key, setKey] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

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
    <AuthCard description="Sign in to manage your Mac compute pool.">
      <CardContent>
        <form onSubmit={submit}>
          <FieldGroup>
            {cfg.oidc && (
              <Field>
                <Button type="button" asChild>
                  <a href={ssoURL(returnTo())}>
                    <LogInIcon data-icon="inline-start" />
                    Sign in with SSO
                  </a>
                </Button>
              </Field>
            )}
            {cfg.oidc && cfg.apikey && <FieldSeparator>or</FieldSeparator>}
            {cfg.apikey && (
              <>
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
                  <Button type="submit" variant={cfg.oidc ? "outline" : "default"} disabled={busy || !key}>
                    {busy ? <Spinner data-icon="inline-start" /> : <KeyRoundIcon data-icon="inline-start" />}
                    Sign in with key
                  </Button>
                </Field>
              </>
            )}
            {!cfg.oidc && !cfg.apikey && (
              <Alert>
                <AlertDescription>Dashboard sign-in is disabled on this server.</AlertDescription>
              </Alert>
            )}
          </FieldGroup>
        </form>
      </CardContent>
    </AuthCard>
  )
}

export function SignedOut({ cfg }: { cfg: AuthConfig }) {
  return (
    <AuthCard description="You are signed out.">
      <CardFooter className="flex-col gap-2">
        <Button className="w-full" asChild>
          <a href={ssoURL("/")}>
            <LogInIcon data-icon="inline-start" />
            Sign in
          </a>
        </Button>
        {cfg.apikey && (
          <Button className="w-full" variant="outline" asChild>
            <a href="/login">
              <KeyRoundIcon data-icon="inline-start" />
              Sign in with API key
            </a>
          </Button>
        )}
      </CardFooter>
    </AuthCard>
  )
}

const reasons: Record<string, string> = {
  expired: "The sign-in took too long, or this browser blocked the sign-in cookie.",
  state: "The sign-in response did not match a request from this browser.",
  provider: "The identity provider did not complete the sign-in.",
  exchange: "The sign-in could not be verified with the identity provider.",
  forbidden: "Your account is not in a group that may use this dashboard. Ask an administrator for access.",
  overage: "Your account is in too many groups for the sign-in token to list them. Ask an administrator to use app roles.",
}

export function SigninError({ cfg }: { cfg: AuthConfig }) {
  const q = new URLSearchParams(location.search)
  const message = reasons[q.get("reason") ?? ""] ?? "Sign-in failed."
  const ret = q.get("return") ?? "/"
  return (
    <AuthCard description="Sign-in did not complete.">
      <CardContent>
        <Alert variant="destructive">
          <CircleAlertIcon />
          <AlertTitle>Could not sign you in</AlertTitle>
          <AlertDescription>{message}</AlertDescription>
        </Alert>
      </CardContent>
      <CardFooter className="flex-col gap-2">
        {cfg.oidc && (
          <Button className="w-full" asChild>
            <a href={ssoURL(ret)}>
              <RotateCwIcon data-icon="inline-start" />
              Try again
            </a>
          </Button>
        )}
        {cfg.apikey && (
          <Button className="w-full" variant="outline" asChild>
            <a href="/login">
              <KeyRoundIcon data-icon="inline-start" />
              Sign in with API key
            </a>
          </Button>
        )}
      </CardFooter>
    </AuthCard>
  )
}
