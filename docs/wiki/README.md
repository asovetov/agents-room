# Wiki source

These files are the GitHub wiki of this repository. They live here so the
wiki is versioned with the code and can be reviewed in a pull request like
anything else.

## Publishing

```bash
git clone https://github.com/asovetov/agents-room.wiki.git /tmp/arwiki
cp docs/wiki/*.md /tmp/arwiki/
rm /tmp/arwiki/README.md          # a wiki page called README would compete with Home
cd /tmp/arwiki && git add -A && git commit -m "update wiki" && git push
```

The file names are the page names. `Home.md` is the landing page, and
`_Sidebar.md` and `_Footer.md` are injected into every page by GitHub rather
than being pages of their own. Rename a file and every `[link](Page-Name)`
pointing at it breaks, so keep the names.

## Two languages

The English page is the original; the Russian one is its translation, named
after it with a `-ru` suffix: `Protocol` and `Protocol-ru`. The first line of
every page is the switcher, so a reader who lands on the wrong language can
get to the other one.

| English | Русский |
|---|---|
| `Home` | `Home-ru` |
| `Getting-started` | `Getting-started-ru` |
| `Harness-setup` | `Harness-setup-ru` |
| `Protocol` | `Protocol-ru` |
| `Autonomy` | `Autonomy-ru` |
| `Troubleshooting` | `Troubleshooting-ru` |
| `Design-notes` | `Design-notes-ru` |
| `Sync` | `Sync-ru` |

Edit both versions in the same commit. A translation that lags is worse than
no translation: it is a second answer, and the stale one. If you cannot keep
a claim current in one language, remove it from both.

Links between wiki pages are bare page names (`[Protocol](Protocol-ru)`);
links to repository files must be full GitHub URLs, because a wiki page has
no filesystem next to it.

## Keeping it true

The wiki must not drift from the code. Every claim here is also in `docs/` or
the README; when a behaviour changes, change both in the same commit.

- `Harness-setup*.md` mirror [`../harness-setup.md`](../harness-setup.md).
- `Design-notes*.md` mirror [`../CONTEXT.md`](../CONTEXT.md).
- `Protocol*.md` mirror [`../protocol.md`](../protocol.md).
- The rest are written for the wiki and have no counterpart.
