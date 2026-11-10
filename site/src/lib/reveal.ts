// Scroll reveal, shared by every section.
//
// Two rules the whole page follows:
//
//   1. Content is visible by default. Motion adds the transition; it never
//      gates whether the text appears. A page that is blank until a script
//      runs is a page that breaks for someone.
//   2. The animation library is `motion/mini`, the animate() core only. The
//      full `motion` entry pulls framer-motion's gesture and layout engine
//      for a page that animates two properties. Watching the viewport is a
//      few lines of IntersectionObserver, and the stagger is a delay per
//      element, so neither needs the larger package.

import { animate } from 'motion/mini';

const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

const RISE = 14;
const DURATION = 0.7;
const STEP = 0.07;

export function reveal(root: ParentNode = document): void {
  if (reduced || !('IntersectionObserver' in window)) return;

  const observer = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        observer.unobserve(entry.target);

        const group = entry.target as HTMLElement;
        const items = group.querySelectorAll<HTMLElement>('[data-reveal-item]');
        const targets = Array.from(items.length > 0 ? items : [group]);
        const start = Number(group.dataset.revealDelay ?? 0);

        targets.forEach((el, i) => {
          animate(
            el,
            { opacity: [0, 1], transform: [`translateY(${RISE}px)`, 'translateY(0px)'] },
            { duration: DURATION, delay: start + i * STEP, ease: [0.16, 1, 0.3, 1] },
          );
        });
      }
    },
    // threshold 0, not 0.2: a group can be taller than the viewport, and 20%
    // of a section taller than the screen is more than the screen can ever
    // show, so the observer would never fire and the text would stay at
    // opacity 0 forever. The negative bottom margin is what gives the reveal
    // its "just as it comes into view" timing without that trap.
    { threshold: 0, rootMargin: '0px 0px -15% 0px' },
  );

  for (const group of root.querySelectorAll<HTMLElement>('[data-reveal]')) {
    const items = group.querySelectorAll<HTMLElement>('[data-reveal-item]');
    for (const el of items.length > 0 ? items : [group]) {
      el.style.opacity = '0';
      el.style.transform = `translateY(${RISE}px)`;
    }
    observer.observe(group);
  }
}
