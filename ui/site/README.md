# tutor-mcp.dev

The public website, built with Next.js as a static export and styled after the
[Tutor-MCP charter](../charte-tutor-mcp.html). Four pages: `/`, `/docs/`,
`/guide/` and `/mentions-legales/`. Everything is in English.

## Build

```sh
npm ci
npm run build      # writes the static site to out/
npm run typecheck  # tsc --noEmit
```

`out/` is committed so the site can be served by any static host without a
Node.js toolchain: copy the directory as is. Routes use trailing slashes and
`out/<route>/index.html`. Rebuild and commit `out/` after changing the sources.

`npm run dev` serves the site locally at http://localhost:3000.

## Content

Copy and versions come from the repository documentation: `README.md`,
`docs/installation.md`, `docs/clients.md`, `docs/profiles.md`,
`docs/algorithms.md` and `client/README.md`. Update `VERSION` and
`CLIENT_VERSION` in `components/ui.tsx` for a new release.

Provider logos in `public/logos/` come from `docs/assets/logos/` (LobeHub
lobe-icons, MIT; brand names remain their owners' property). Fonts are
self-hosted by `next/font` at build time.
