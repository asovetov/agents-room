# Wiki source

These files are the GitHub wiki of this repository. They live here so the
wiki is versioned with the code and can be reviewed in a pull request like
anything else.

## Publishing

GitHub creates the wiki repository only after the first page is made through
the web interface. Once that exists, publishing is one push:

```bash
git clone https://github.com/asovetov/agents-room.wiki.git /tmp/arwiki
cp docs/wiki/*.md /tmp/arwiki/
cd /tmp/arwiki && git add -A && git commit -m "update wiki" && git push
```

The file names are the page names: `Home.md` is the landing page, and the
others are reachable as `/wiki/Protocol`, `/wiki/Troubleshooting` and so on.
Rename a file and every `[link](Page-Name)` pointing at it breaks, so keep
the names.

## Keeping it true

The wiki must not drift from the code. Every claim here is also in `docs/` or
the README; when a behaviour changes, change both in the same commit.

- `Harness-setup.md` is a copy of [`../harness-setup.md`](../harness-setup.md).
- `Design-notes.md` is a copy of [`../CONTEXT.md`](../CONTEXT.md).
- The rest are written for the wiki and have no counterpart.

The README is excluded from the published set: a wiki page called README
would compete with Home as the landing page.
