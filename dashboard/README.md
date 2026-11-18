# use-my-mac dashboard

React + Vite + official [shadcn/ui](https://ui.shadcn.com) components. It is served by `umm-server` from
`web_dir`.

```sh
npm ci
npm run dev     # http://localhost:5173, proxies /v1 and /auth to a umm-server on :8080
npm run build   # dist/
npm run lint
```

Add UI components only through the shadcn CLI (`npx shadcn@latest add <component>`). The files in
`src/components/ui` are vendored registry code.
