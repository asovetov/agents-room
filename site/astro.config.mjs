// @ts-check
import { defineConfig } from 'astro/config';
import tailwindcss from '@tailwindcss/vite';

// The site is served from https://asovetov.github.io/agents-room/, so every
// absolute URL needs the repository name in front of it. Astro prepends
// `base` to assets it manages; `withBase()` in src/lib/url.ts does the same
// for the links written by hand.
export default defineConfig({
  site: 'https://asovetov.github.io',
  base: '/agents-room',
  trailingSlash: 'ignore',
  vite: {
    plugins: [tailwindcss()],
  },
});
