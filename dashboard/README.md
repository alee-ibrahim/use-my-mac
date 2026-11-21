# use-my-mac dashboard

React + Vite + official [shadcn/ui](https://ui.shadcn.com) components. It is served by `umm-server` from
`web_dir`.

```sh
pnpm install
pnpm dev        # http://localhost:5173, proxies /v1 and /auth to a umm-server on :8080
pnpm build      # dist/
pnpm lint
```

Add UI components only through the shadcn CLI (`pnpm dlx shadcn@latest add <component>`). The files in
`src/components/ui` are vendored registry code.
