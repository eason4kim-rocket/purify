# Purify Search — Homepage Visual Lock V1

Static, responsive implementation baseline for the agreed four-act homepage direction.
Motion is intentionally paused until the visual lock is approved.

## Preview

Serve `creative/site-v1` as the web root, then open `/prototype/`:

```bash
cd creative/site-v1
python3 -m http.server 4174
```

Open `http://127.0.0.1:4174/prototype/`.

The default URL shows the contained Hero B experiment with the locked Signal Blue palette. Append
`?hero=a` to compare it with the committed full-bleed Hero A baseline.

Brand and data colors are defined through the primitive ramp and semantic roles documented in
[`../COLOR-SYSTEM-V1.md`](../COLOR-SYSTEM-V1.md).

## Included

- 12 / 8 / 4-column responsive layout
- Self-hosted Newsreader, Geist, Geist Mono, Noto Serif SC, and Noto Sans SC
- English and Simplified Chinese copy switch
- Automatic light / dark theme with a persistent manual switch
- Login prototype with Google, GitHub, and email entry points
- Continuous light or near-black page surface with a deterministic Signal Blue photon field
- Canvas-integrated optical core that illuminates the photon field itself instead of covering it
- Light-only photons with one visible, reversible four-point star morph at a time
- Nature, human measure, living evidence, and future chapters
- Single-slot Search / Assimilation / Self-healing editorial visual selector
- Illustrative evidence record with explicit uncertainty
- Accessible menu, focus states, image alt text, and reduced-motion fallback
- Static 1440 px and 390 px long-page review renders in `previews/`

The documentary images are generated visual-direction assets, not depictions of real customers or events. The World in Motion image remains a placeholder for the future film.

The first visit follows the operating-system color preference. The header theme control stores an
explicit choice locally, while both themes keep the same locked Signal Blue brand ramp.

The source of truth for typography, spacing, imagery, copy hierarchy, and CTA treatment is
`../VISUAL-LOCK-V1.md`.
