# tutor-mcp.dev

The public Tutor MCP website, based on the interface and visual charter from
`ui/charte-graphique`. Next.js exports plain files for the existing OVH Apache
hosting. The site and its documentation are in English, as the charter specifies.

## Visitor journeys

- `/` introduces Tutor MCP and Tutor ACP, personal learning, institutions and
  course sharing. Product illustrations use clearly labelled example data.
- `/download/` separates the Windows x64 desktop application from the six server
  builds, with architecture selection, checksums and copyable installer commands.
- `/docs/` offers three starting points: join an institution, learn locally or
  operate a service. Ten focused guides cover installation, the first session,
  institutions, sharing, backups and troubleshooting.
- `/guide/` walks a beginner through a useful learning session.
- `/mentions-legales/` retains the publisher and OVH hosting information.

The site has no registration or hosted Tutor account service. AI provider access
is separate from the open-source software. The client currently ships for
Windows x64; Linux, macOS and Windows server binaries are separate downloads.

## Build and check

Requirements: Node.js 22.12+ and Python 3.9+ for export validation and preview.

```sh
npm ci
npm run build
npm run typecheck
npm test
npm run check:export
npm run test:browser
```

Browser checks use `/usr/bin/google-chrome` locally, or `CHROME_BIN` if set.
For another platform, install the Playwright browser and provide its executable,
or use the same Chromium setup as `.github/workflows/website.yml`. CI installs
Playwright Chromium and runs the checks on the generated static site.

`npm run preview` serves `out/` at `http://127.0.0.1:18765`.
`npm run dev -- --port 18766` runs the development server.

The browser suite checks every public page at desktop and mobile sizes, native
mobile navigation, code copying, platform selection, release refresh and API
fallback. The export check validates local links, fragments, assets, metadata,
Apache configuration and the installer copies. These are automated checks, not
a claim of a complete manual accessibility audit.

`out/` is committed so a static host needs no Node.js installation. Rebuild it
when source content changes. Browser screenshots and test results are ignored.

## Keep downloads current

```sh
npm run sync-releases
npm run build
```

The sync script verifies the latest stable GitHub release and updates
`lib/release-data.json`. It refuses an invalid response and preserves the previous
verified snapshot. The downloads page also checks the public GitHub API in the
browser; it keeps verified links if the API is unavailable or rate limited.
Server and ACP product versions are resolved independently. A server-only release
can still use the most recently distributed stable ACP installer. Technical
client prereleases are not selected automatically.

Release data is public and needs no token. Review the release notes and the
installation guides when a version changes; dynamic links do not make new
installation procedures automatically compatible.

## Content sources

The primary product references are the repository `README.md`, `client/README.md`,
`docs/tutor-conversation-formations.md`, `docs/profiles.md`,
`docs/installation.md` and `docs/learning-integrity.md`. Detailed articles link
to their source documents. Learning guides are in `lib/learning-guides.tsx`;
installation guides are in `lib/installation-guides.tsx`.

The native institution connection uses locally generated public-key identities
and the service origin without `/mcp`. Hobby OAuth clients use their `/mcp`
endpoint. Historical institution phase documents must not replace the current
client workflow. Shared programmes copy the course design, keeping private
conversation and progress separate; enrolled versions remain stable.

## Publish to OVH

1. Back up the current remote directory and verify the target domain.
2. Run the production build and all relevant checks above.
3. Upload the **contents** of `out/`, including `.htaccess`. Upload hashed assets
   first and use temporary files plus rename for HTML replacement. Preserve old
   hashed assets during the transition so cached pages remain usable.
4. Verify the public home page, downloads, deep guide URLs, CSS/JS and the 404.

Only generated public files belong on the web host. FTP connection settings stay
outside this checkout and outside `out/`; builds require no `.env`.

Repository publication follows the root `AGENTS.md` workflow: explicit task-owned
files, verification and CI on `staging`, then fast-forward promotion to `main`.
This Git operation does not itself deploy the OVH site.

Provider logos retain their original licence in `public/logos/LICENSE`. Fonts
are self-hosted in the export by `next/font` at build time.
