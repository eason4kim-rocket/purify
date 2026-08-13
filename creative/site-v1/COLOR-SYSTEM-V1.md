# Purify Search — Color System V1

Status: implemented in the homepage prototype.

Purify does not use one blue everywhere. A ten-step primitive ramp supplies semantic roles for
brand, interaction, evidence, data, focus, and the signature Hero stage. Components consume the
semantic tokens, never the raw scale directly.

## 1. Purify Blue primitives

| Token | Value | Primary purpose |
|---|---:|---|
| `--purify-blue-10` | `#EDF4FF` | Highlight data points |
| `--purify-blue-20` | `#D8E7FF` | Subtle brand surfaces |
| `--purify-blue-30` | `#AECFFF` | Borders and muted data |
| `--purify-blue-40` | `#7AAAFF` | Default data marks |
| `--purify-blue-50` | `#4381FF` | Strong data marks |
| `--purify-blue-60` | `#1765FF` | Default brand accent and focus |
| `--purify-blue-70` | `#0052E0` | Emphasis and signature stage |
| `--purify-blue-80` | `#003DB0` | Hover and brand text |
| `--purify-blue-90` | `#002C80` | Active state and stage depth |
| `--purify-blue-100` | `#001B4D` | Deep Ocean surface |

## 2. Semantic roles

| Role | Light theme | Dark theme |
|---|---|---|
| Brand default | Blue 60 | Blue 50 |
| Brand emphasis / primary CTA | Blue 70 | Blue 60 |
| Brand hover | Blue 80 | Blue 50 |
| Brand active | Blue 90 | Blue 70 |
| Brand surface | Blue 10 | `#071A3D` |
| Brand subtle surface | Blue 20 | `#0B2B61` |
| Brand border | Blue 30 | `#2459AA` |
| Brand foreground | Blue 80 | `#86ADFF` |
| Focus ring | Blue 60 | Blue 40 |
| Hero stage | Blue 70 | Blue 70 |
| Data highlight / quiet / muted / default / strong / depth | Blue 10 / 20 / 30 / 40 / 50 / 80 | Blue 10 / 20 / 30 / 40 / 50 / 80 |

`Memory Indigo` remains a separate narrative accent and must not replace the brand scale. It is
reserved for the Living Memory chapter.

## 3. Hero field distribution

The Hero uses one stable Blue 70 field. Its data-square distribution is deliberately weighted:

- 8% highlight
- 16% quiet
- 28% muted
- 26% default
- 15% strong
- 7% depth

This prevents the stage from becoming pale while retaining enough local contrast for the matrix
to remain visible. The bottom flare is rendered inside the matrix canvas rather than laid over it
as a CSS gradient: nearby squares brighten, enlarge, and pick up ice-blue additive light. The broad
surrounding haze remains subtle and returns immediately to the Signal Blue ramp.

## 4. Rules

1. Raw `--purify-blue-*` tokens are primitives; components use `--color-*` roles.
2. Large brand fields use one dominant blue, not a multicolor gradient.
3. White text and controls over brand surfaces must retain WCAG AA contrast.
4. Color never carries evidence state by itself; labels, shape, and status text remain present.
5. Dark mode remaps semantic roles while retaining the same primitive brand ramp; near-black
   surfaces use a restrained blue undertone rather than neutral charcoal cards.

## 5. Theme surfaces

The light theme uses Paper `#F7F8F4` and Pure White `#FCFCF9`. The dark theme is intentionally one
continuous deep blue-black field (`#0A111B`) so chapters do not alternate between unrelated black
and white panels. Elevated evidence records use `#111B29`; hairlines become translucent cool-white.

Documentary photographs are gently dimmed in dark mode. Pale abstract evidence images receive a
controlled tonal inversion so their material texture remains visible without creating bright white
rectangles. The signature Signal Blue Hero stage is identical in both modes.

## 6. Locked palette

Signal Blue is the selected Purify brand ramp. The former Mineral, Cobalt, Ultramarine, and Abyss
studies and the prototype palette switcher have been removed. Production now has one color source
of truth.

References: Carbon color ramps and role tokens, Primer semantic color usage, and WCAG 2.2 contrast
guidance.
