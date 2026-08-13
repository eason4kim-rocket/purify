# Purify Search — Visual Lock V1

Status: implementation baseline, scale refinement V1.1. Motion is intentionally paused.

## 1. Typography

Purify uses three functional type roles rather than one font everywhere.

| Role | English | Simplified Chinese | Use |
|---|---|---|---|
| Display | Newsreader Variable | Noto Serif SC Variable | Hero, chapter titles, manifesto lines |
| Interface | Geist Variable | Noto Sans SC Variable | Navigation, body copy, captions, CTA |
| Evidence | Geist Mono Variable | Geist Mono Variable | Source, state, time, section index |

All five fonts are self-hosted under `assets/fonts/`. The Chinese fonts are page-specific
WOFF2 subsets; they must be regenerated when new Chinese copy is introduced. Font licenses
are stored beside the font files.

The choice follows the strongest pattern in the reviewed AI and developer sites: a custom or
distinctive display voice, a restrained UI sans, and a separate technical mono. OpenAI Sans,
Anthropic Sans, Söhne, GT America, Reckless, and other proprietary brand faces are references,
not assets to copy. Newsreader + Geist gives Purify the same role separation with an open,
commercially usable stack.

Type scale at 1440 px after the V1.1 density refinement:

- Hero: 74 px maximum, 0.97 line-height.
- Chapter title: 56 px maximum, 1.04 line-height.
- Manifesto / quotation: 40 px maximum.
- Lead: 18 px.
- Body: 16 px.
- Evidence label: 11 px Geist Mono.

At 390 px, Hero is 38 px in English / 34 px in Chinese, chapter titles are 32 px / 30 px,
and body copy remains at least 16 px. The smaller display scale creates hierarchy without
turning every section into a full-screen poster.

## 2. Grid and whitespace

- Desktop canvas: 1440 px reference viewport.
- Content maximum: 1200 px, producing 120 px outer margins at 1440 px.
- Desktop: 12 columns, 24 px gutters.
- Tablet: 8 columns, 20 px gutters.
- Mobile: 4 columns, 20 px outer margins, 12 px gutters.
- Section space: 144 px desktop, 112 px tablet, 80 px mobile.
- Chapter-to-primary-visual gap: 80 px desktop, 64 px tablet, 44 px mobile.
- Major intra-chapter hand-off: 120 px desktop, 96 px tablet, 72 px mobile.

Media uses its own narrower measure inside the grid: the origin and memory fields stop at
1080 px, the evidence specimen at 1120 px, and the evidence state image at 520 px. On mobile,
documentary images keep landscape proportions instead of being enlarged into portrait cards.

Whitespace is structural. No decorative card grid is added merely to fill empty areas.

## 3. Copy hierarchy

Every chapter has exactly four possible levels:

1. 11 px mono index and kicker.
2. One display title.
3. One lead paragraph.
4. Optional caption or evidence label attached to media.

The Hero contains one proposition, one explanation, and two actions. Middle narrative acts
contain no conversion CTA. The evidence record gets one contextual text link only.

On desktop the complete Hero copy group carries a 42–52 px optical downward offset. This keeps
the title below the densest part of the square field and uses the lower white space deliberately.
Tablet reduces the offset to 34 px and mobile to 18 px.

## 4. CTA hierarchy

- Primary action: `Build with Purify Search` / `使用 Purify Search 构建`.
- It appears only in the Hero and final act, with identical destination and solid treatment.
- Secondary Hero action: `Read our vision` as a text link.
- Secondary final action: `Follow our work` as a text link.
- Navigation `Docs` remains a utility link, not a competing filled button.

## 5. Image language

There are only two image families:

1. Documentary reality: wide natural-light images of the physical world and human observation.
2. Mineral evidence: square, tactile paper/water/mineral abstractions for Search, Assimilation,
   and Self-healing.

The flat Purify Matrix is graphic grammar, not a third photo style. It is allowed in the Hero
and once in Living Memory. Photography carries natural color; no global blue wash is applied.
Titles remain outside images. Image radii are 8 px; the evidence record uses 12 px.

## 6. Static lock

`VISUAL_LOCK_STATIC` is enabled in `prototype/app.js`, and `<html>` carries
`visual-lock-v1`. Staged reveals, scroll transforms, video playback, and long transitions remain
paused. The Hero square field is the one deliberate exception: its ambient Canvas loop stays live
so density, color depth, and motion can be judged in context. It still becomes static for system
reduced-motion preferences and capture mode. Tabs and language switching remain available for
layout QA.

Motion work resumes only after the 1440 px desktop and 390 px mobile long pages are approved.
