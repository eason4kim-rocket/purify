# Purify brand film

Working title: **The World Becomes Signal**

This directory contains the production files for a 36-second, wordless Purify
brand film. The film braids three visible strands from its first frame:

- nature: ocean life and the physical world;
- humanity: care, attention, and exploration;
- data/technology: instruments and the signals they reveal.

The editorial rhythm borrows the lightness of Anthropic's brand films without
copying their visual assets. Meaning is created through hard cuts, matched
shapes, matched actions, and tactile data rather than narration or on-screen
copy.

## Delivery targets

- `purify-signal-film-1080p.mp4`: 1920x1080, 30 fps, 36 seconds, stereo music.
- `purify-signal-film-review-1080p.mp4`: smaller 1080p review copy with the
  same stereo music.
- `purify-signal-film-web-bg-1080p.mp4`: 1920x1080, 30 fps, muted, fast-start.
- `purify-signal-score.wav`: 48 kHz, 24-bit stereo original score.

The standalone film is composed first. A web-background derivative retains a
quiet left-side copy-safe area instead of weakening the main edit.

The current MP4 is the **v1 picture-and-sound cut**: approved generated style
frames, procedural camera movement, tactile signal animation, and the finished
original score. It is intentionally the stage at which pacing and image
language are approved before each still-based section is replaced by a full
motion shot.

## Visual rules

- No titles, subtitles, labels, logos, spoken language, or legible UI text.
- No neon gradients, code rain, floating HUDs, glowing brains, or generic AI
  networks.
- Natural daylight, ivory, ocean teal, graphite, true skin tones, and a small
  amount of mineral blue.
- Subjects favor the right 55–88% of the frame; the left 0–38% remains calm
  enough for later website copy.
- Data is photographed or composited as material: glass, translucent film,
  physical displays, light, and sparse signal marks.
- Point imagery is brief and sparse; it never becomes the film's dominant
  visual language.

## Rhythm

- 100 BPM, 4/4, 15 bars, exactly 36 seconds.
- 30 fps: 18 frames per beat, 72 frames per bar, 1080 frames total.
- Each bar has one dominant visual idea, with internal action landing on beats.
- Bar 11 intentionally drops a beat; bars 14–15 resolve without a trailer-style
  climax.

## Rebuild

```bash
python3 scripts/compose_music.py
python3 scripts/render_animatic.py
```

The renderer uses the static FFmpeg bundled with `imageio-ffmpeg`, avoiding the
currently broken Homebrew FFmpeg linkage on this machine.
