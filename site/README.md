# agents-room landing page

The one-page site at <https://asovetov.github.io/agents-room/>.

Astro + Tailwind CSS + Motion, static output. Nothing here is needed to build
or use agents-room itself; the Go project does not depend on this directory.

## Run it

```bash
cd site
npm ci
npm run dev      # http://localhost:4321/agents-room/
```

```bash
npm run build    # static output in site/dist
npm run check    # type-check the .astro files
```

`astro.config.mjs` sets `base: '/agents-room'`, so the dev server and the
build both serve under that path — the same path GitHub Pages uses. A link
written as `/foo` will 404 in both; use `${import.meta.env.BASE_URL}` or a
relative path.

## Where things are

| File | What it holds |
|---|---|
| `src/lib/content.ts` | Every string, English and Russian side by side |
| `src/components/` | One component per section |
| `src/styles/global.css` | Colour tokens, type scale, the few shared classes |
| `src/lib/reveal.ts` | The only client-side script: scroll reveal |

Both languages render the same components through `src/components/Landing.astro`,
so a layout change happens once. Adding a string means adding it to both
`en` and `ru` in `content.ts` — the `Copy` type makes a missing translation a
build error rather than a blank space on the live site.

## Two rules the page follows

1. **Content is visible without JavaScript.** Motion adds a transition; it
   never decides whether text appears. `prefers-reduced-motion` skips it
   entirely.
2. **No layout is scaled down from another.** The hero diagram has a separate
   narrow version rather than a shrunken desktop one, because labels at 5 px
   are not a small diagram, they are an unreadable one.

## Deploy

`.github/workflows/pages.yml` builds on every push to `main` that touches
`site/` and publishes the artifact. Pages must be set to **Source: GitHub
Actions** in the repository settings once; after that no branch is involved.
