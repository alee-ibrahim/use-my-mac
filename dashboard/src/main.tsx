import { lazy, StrictMode, Suspense } from "react"
import { createRoot } from "react-dom/client"

import "./index.css"
import App from "./App.tsx"
import { ThemeProvider } from "@/components/theme-provider.tsx"

// The docs (and their Markdown renderer) load only on /docs.
const DocsApp = lazy(() =>
  import("@/pages/docs").then((m) => ({ default: m.DocsApp }))
)
const onDocs =
  location.pathname === "/docs" || location.pathname.startsWith("/docs/")

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider>
      {onDocs ? (
        <Suspense>
          <DocsApp />
        </Suspense>
      ) : (
        <App />
      )}
    </ThemeProvider>
  </StrictMode>
)
