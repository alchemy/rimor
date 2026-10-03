# rimor.dev

The project page, served by Cloudflare Pages from this folder as plain
files: no build step.

## The screens

The three screens on the page are the real interface, drawn by rimor's own
code and converted from terminal escape codes to HTML. After changing the
UI, regenerate them from the repository root:

```sh
RIMOR_SITE=site go test ./internal/ui -run TestSiteScreens
```

The test builds a small SQLite database, drives rimor into the state shown,
and writes each theme's screen into `index.html` between its
`<!-- screen:NAME -->` markers. The `terminal` screen uses the Gruvbox
palette, chosen to look unlike the dark theme.

## Go import path

`rimor.dev` is also the Go module path. `go install rimor.dev/cmd/rimor`
asks `https://rimor.dev/cmd/rimor?go-get=1` for a `go-import` tag, which
`cmd/rimor.html` provides (Pages serves it at `/cmd/rimor`); `index.html`
carries the same tag for the module root. Keep both in step if the
repository moves.

## Deploying

Cloudflare Pages, connected to the GitHub repository. In the setup wizard:

- Framework preset: None
- Build command: empty
- Build output directory: `site`
- Root directory: the repository root

After the first deploy, under the project's Settings → Builds, set *Build
watch paths* to include `site/*`, so commits that only change code do not
redeploy the page. Without it every push redeploys the same files, which is
harmless.
