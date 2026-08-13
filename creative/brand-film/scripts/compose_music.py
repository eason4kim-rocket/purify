#!/usr/bin/env python3
"""Compose the original 36-second score for the Purify signal film.

All sound is synthesized locally. There are no samples, voices, or copied
recordings. The output is deterministic so picture edits can remain locked to
the 100 BPM grid.
"""

from __future__ import annotations

from pathlib import Path

import numpy as np
import soundfile as sf
from scipy import signal


SR = 48_000
BPM = 100.0
BEAT = 60.0 / BPM
BAR = 4.0 * BEAT
BARS = 15
DURATION = BARS * BAR
N = int(round(DURATION * SR))
ROOT = Path(__file__).resolve().parents[1]
AUDIO_DIR = ROOT / "assets" / "audio"
RNG = np.random.default_rng(260808)


def stereo() -> np.ndarray:
    return np.zeros((N, 2), dtype=np.float64)


def equal_power_pan(mono: np.ndarray, pan: float) -> np.ndarray:
    """Pan mono in [-1, 1] using equal-power gains."""
    angle = (np.clip(pan, -1.0, 1.0) + 1.0) * np.pi / 4.0
    return np.column_stack((mono * np.cos(angle), mono * np.sin(angle)))


def place(track: np.ndarray, sound: np.ndarray, start: float, gain: float = 1.0) -> None:
    begin = int(round(start * SR))
    if begin >= N or begin + len(sound) <= 0:
        return
    source_start = max(0, -begin)
    target_start = max(0, begin)
    count = min(len(sound) - source_start, N - target_start)
    if count > 0:
        track[target_start : target_start + count] += sound[source_start : source_start + count] * gain


def exp_env(length: int, attack: float, decay: float) -> np.ndarray:
    attack_n = max(1, int(round(attack * SR)))
    idx = np.arange(length)
    rise = np.minimum(1.0, idx / attack_n)
    fall = np.exp(-np.maximum(0, idx - attack_n) / max(1.0, decay * SR))
    return np.sin(rise * np.pi / 2.0) ** 2 * fall


def colored_noise(duration: float, low: float, high: float) -> np.ndarray:
    length = int(round(duration * SR))
    noise = RNG.normal(0.0, 1.0, length)
    sos = signal.butter(3, [low, high], btype="bandpass", fs=SR, output="sos")
    return signal.sosfilt(sos, noise)


def ocean_resonance(duration: float = 2.25, pan: float = 0.0, variant: int = 0) -> np.ndarray:
    length = int(round(duration * SR))
    t = np.arange(length) / SR
    phases = np.array([0.0, 0.42, 1.18]) + variant * 0.17
    pivot = 0.46 + variant * 0.015
    f0 = np.where(
        t < duration * pivot,
        73.42 + (110.0 - 73.42) * (t / (duration * pivot)),
        110.0 + (82.41 - 110.0) * ((t - duration * pivot) / (duration * (1.0 - pivot))),
    )
    phase = 2.0 * np.pi * np.cumsum(f0) / SR
    body = (
        np.sin(phase + phases[0])
        + 0.24 * np.sin(2.03 * phase + phases[1])
        + 0.10 * np.sin(3.91 * phase + phases[2])
    )
    body *= 1.0 + 0.055 * np.sin(2.0 * np.pi * 0.18 * t)
    air = colored_noise(duration, 420.0, 1_250.0)
    air /= max(1e-9, np.max(np.abs(air)))
    mono = (0.72 * body + 0.055 * air) * exp_env(length, 0.18, 1.1)
    return equal_power_pan(mono, pan)


def heartbeat(pan: float = 0.0, strength: float = 1.0) -> np.ndarray:
    duration = 0.43
    length = int(round(duration * SR))
    t = np.arange(length) / SR
    mono = np.zeros(length)
    for offset, freq, amp, decay in ((0.0, 62.0, 1.0, 0.075), (0.128, 47.0, 0.66, 0.105)):
        start = int(round(offset * SR))
        tt = t[: length - start]
        env = np.exp(-tt / decay) * (1.0 - np.exp(-tt / 0.004))
        thump = np.sin(2.0 * np.pi * freq * tt) + 0.22 * np.sin(2.0 * np.pi * 2.1 * freq * tt)
        mono[start:] += amp * thump * env
    contact = colored_noise(duration, 110.0, 620.0) * exp_env(length, 0.001, 0.028)
    mono = (mono + 0.035 * contact) * strength
    return equal_power_pan(mono, pan)


def pulsar_click(note_hz: float = 3_050.0, pan: float = 0.0, strength: float = 1.0) -> np.ndarray:
    duration = 0.18
    length = int(round(duration * SR))
    t = np.arange(length) / SR
    ring = np.sin(2.0 * np.pi * note_hz * t) * np.exp(-t / 0.026)
    noise = RNG.normal(0.0, 1.0, length)
    noise *= np.exp(-t / 0.004)
    mono = strength * (0.28 * ring + 0.055 * noise)
    return equal_power_pan(mono, pan)


def glass_tone(fundamental: float, pan: float = 0.0, strength: float = 1.0) -> np.ndarray:
    duration = 1.35
    length = int(round(duration * SR))
    t = np.arange(length) / SR
    ratios = (1.0, 2.756, 5.404, 8.933)
    decays = (0.82, 0.51, 0.29, 0.18)
    amps = (1.0, 0.34, 0.16, 0.07)
    mono = np.zeros(length)
    for ratio, decay, amp in zip(ratios, decays, amps):
        mono += amp * np.sin(2.0 * np.pi * fundamental * ratio * t) * np.exp(-t / decay)
    excite = RNG.normal(0.0, 1.0, length) * np.exp(-t / 0.003)
    mono = strength * (0.18 * mono + 0.018 * excite)
    return equal_power_pan(mono, pan)


def organic_brush(duration: float = 0.5, pan_start: float = -0.4, pan_end: float = 0.4) -> np.ndarray:
    length = int(round(duration * SR))
    noise = colored_noise(duration, 520.0, 4_200.0)
    noise /= max(1e-9, np.max(np.abs(noise)))
    t = np.linspace(0.0, 1.0, length, endpoint=False)
    env = np.sin(np.pi * np.clip(t, 0.0, 1.0)) ** 1.8
    flutter = 0.58 + 0.42 * np.sin(2.0 * np.pi * (7.0 * t + 1.4 * t * t)) ** 2
    mono = 0.09 * noise * env * flutter
    pans = np.linspace(pan_start, pan_end, length)
    angles = (pans + 1.0) * np.pi / 4.0
    return np.column_stack((mono * np.cos(angles), mono * np.sin(angles)))


def airy_pad(start_freq: float, end_freq: float, duration: float, pan: float = 0.0, gain: float = 0.08) -> np.ndarray:
    length = int(round(duration * SR))
    t = np.arange(length) / SR
    freq = np.linspace(start_freq, end_freq, length)
    phase = 2.0 * np.pi * np.cumsum(freq) / SR
    mono = (
        np.sin(phase)
        + 0.25 * np.sin(2.0 * phase + 0.3)
        + 0.12 * np.sin(3.0 * phase + 1.1)
    )
    env = np.sin(np.pi * np.clip(t / duration, 0.0, 1.0)) ** 0.72
    mono *= env * gain
    return equal_power_pan(mono, pan)


def add_short_reverb(track: np.ndarray, wet: float = 0.12) -> np.ndarray:
    ir_len = int(round(1.05 * SR))
    t = np.arange(ir_len) / SR
    base = RNG.normal(0.0, 1.0, ir_len) * np.exp(-t / 0.34)
    base[: int(0.012 * SR)] = 0.0
    left_ir = base.copy()
    right_ir = np.roll(base, int(0.007 * SR)) * 0.94
    left_ir[0] += 1.0
    right_ir[0] += 1.0
    left_ir /= np.sqrt(np.sum(left_ir**2)) + 1e-9
    right_ir /= np.sqrt(np.sum(right_ir**2)) + 1e-9
    verb_l = signal.fftconvolve(track[:, 0], left_ir, mode="full")[:N]
    verb_r = signal.fftconvolve(track[:, 1], right_ir, mode="full")[:N]
    verb = np.column_stack((verb_l, verb_r))
    verb /= max(1e-9, np.max(np.abs(verb)))
    return track + wet * verb


def main() -> None:
    AUDIO_DIR.mkdir(parents=True, exist_ok=True)
    ocean = stereo()
    pulse = stereo()
    data = stereo()
    texture = stereo()
    harmony = stereo()

    # Ocean-like calls: synthetic resonances, never a sampled animal recording.
    for bar, beat_offset, pan, gain in (
        (0, 0.08, -0.08, 0.45),
        (2, 0.18, 0.16, 0.34),
        (6, 0.10, -0.12, 0.23),
        (9, 0.00, 0.00, 0.20),
        (11, 0.00, 0.00, 0.17),
        (13, 0.02, 0.00, 0.24),
        (14, 0.00, 0.00, 0.31),
    ):
        place(ocean, ocean_resonance(2.15, pan, bar), bar * BAR + beat_offset, gain)

    # Human pulse: gradually enters, becomes active, drops out, then returns.
    pulse_events: list[tuple[float, float, float]] = []
    for bar in range(1, 5):
        for beat in (0, 2):
            pulse_events.append((bar * BAR + beat * BEAT, 0.0, 0.34))
    for bar in range(5, 10):
        beats = (0, 1, 2, 3) if bar >= 7 else (0, 2, 3)
        for beat in beats:
            pulse_events.append((bar * BAR + beat * BEAT, -0.04 + 0.025 * beat, 0.30 + 0.035 * (beat % 2)))
    # Bar 11 (index 10) intentionally misses beat 3 (index 2).
    for beat in (0, 1, 3):
        pulse_events.append((10 * BAR + beat * BEAT, 0.0, 0.28))
    for beat in (0, 1):
        pulse_events.append((11 * BAR + beat * BEAT, 0.0, 0.22))
    pulse_events.extend(
        [
            (12 * BAR + 1 * BEAT, 0.0, 0.31),
            (13 * BAR + 0 * BEAT, 0.0, 0.33),
            (13 * BAR + 2 * BEAT, 0.0, 0.34),
            (14 * BAR + 0 * BEAT, 0.0, 0.36),
            (14 * BAR + 2 * BEAT, 0.0, 0.37),
            (14 * BAR + 3.25 * BEAT, 0.0, 0.28),
        ]
    )
    for idx, (when, pan, gain) in enumerate(pulse_events):
        jitter = 0.0 if when >= 12 * BAR else RNG.uniform(-0.006, 0.006)
        place(pulse, heartbeat(pan, 1.0 + RNG.uniform(-0.06, 0.06)), when + jitter, gain)

    # Precise radio-data clicks. Notes loosely spell D-E-A-B.
    click_notes = (293.66, 329.63, 440.0, 493.88)
    click_multipliers = (9.4, 9.2, 7.2, 6.4)
    for bar in range(BARS):
        if bar < 2:
            beats = (3,)
        elif bar < 5:
            beats = (0.5, 2.0, 3.25)
        elif bar < 10:
            beats = (0.25, 1.5, 2.5, 3.5)
        elif bar == 10:
            beats = (0.4, 0.5, 1.8, 1.9, 3.3)
        elif bar == 11:
            beats = (0.2, 1.0)
        else:
            beats = (0.5, 1.5, 2.5, 3.5)
        for i, beat in enumerate(beats):
            note = click_notes[(bar + i) % len(click_notes)] * click_multipliers[(bar + i) % 4]
            pan = (-0.62, 0.58, -0.25, 0.35)[(bar + i) % 4]
            gain = 0.12 if bar < 5 else 0.15
            place(data, pulsar_click(note, pan), bar * BAR + beat * BEAT, gain)

    # Glass notes reveal the four-note signal without turning into an arpeggio.
    glass_events = (
        (3 * BAR + 0.0 * BEAT, 293.66, -0.32, 0.42),
        (4 * BAR + 0.2 * BEAT, 293.66, -0.45, 0.31),
        (4 * BAR + 1.2 * BEAT, 329.63, 0.35, 0.27),
        (4 * BAR + 2.4 * BEAT, 440.00, -0.18, 0.24),
        (4 * BAR + 3.2 * BEAT, 493.88, 0.46, 0.22),
        (8 * BAR + 0.0 * BEAT, 293.66, -0.38, 0.25),
        (8 * BAR + 1.0 * BEAT, 329.63, 0.30, 0.23),
        (8 * BAR + 2.25 * BEAT, 440.00, -0.15, 0.20),
        (8 * BAR + 3.25 * BEAT, 493.88, 0.42, 0.19),
        (12 * BAR + 3.0 * BEAT, 293.66, -0.25, 0.23),
        (13 * BAR + 0.5 * BEAT, 293.66, -0.45, 0.30),
        (13 * BAR + 1.5 * BEAT, 329.63, 0.20, 0.27),
        (13 * BAR + 2.5 * BEAT, 440.00, -0.10, 0.24),
        (13 * BAR + 3.5 * BEAT, 493.88, 0.38, 0.22),
        (14 * BAR + 2.0 * BEAT, 659.25, 0.18, 0.23),
        (14 * BAR + 2.9 * BEAT, 987.77, 0.44, 0.17),
    )
    for when, freq, pan, gain in glass_events:
        place(harmony, glass_tone(freq, pan), when, gain)

    for bar in (3, 5, 7, 8, 9, 10, 11, 13):
        duration = 0.42 if bar != 11 else 0.70
        brush = organic_brush(duration, -0.52, 0.50)
        if bar == 11:
            brush = brush[::-1].copy()
        place(texture, brush, bar * BAR + (1.15 if bar % 2 else 2.2) * BEAT, 1.0)

    # Open, non-triumphant harmonic field for the last three bars.
    place(harmony, airy_pad(73.42, 73.42, 8.3, -0.12, 0.070), 12 * BAR - 0.35, 1.0)
    place(harmony, airy_pad(220.0, 220.0, 7.8, 0.18, 0.040), 12 * BAR, 1.0)
    place(harmony, airy_pad(329.63, 329.63, 4.6, 0.28, 0.028), 13 * BAR, 1.0)

    stems = {
        "score-ocean.wav": ocean,
        "score-pulse.wav": pulse,
        "score-data.wav": data,
        "score-texture.wav": texture,
        "score-harmony.wav": harmony,
    }
    mix = ocean + pulse + data + texture + harmony
    mix = add_short_reverb(mix, wet=0.10)

    # Remove DC and tame subsonic energy while preserving the physical low end.
    sos = signal.butter(2, 28.0, btype="highpass", fs=SR, output="sos")
    mix = signal.sosfilt(sos, mix, axis=0)
    fade_in = int(0.10 * SR)
    fade_out = int(0.15 * SR)
    mix[:fade_in] *= np.linspace(0.0, 1.0, fade_in)[:, None]
    mix[-fade_out:] *= np.linspace(1.0, 0.0, fade_out)[:, None]
    peak = np.max(np.abs(mix))
    mix *= (10.0 ** (-1.5 / 20.0)) / max(peak, 1e-9)

    for name, stem in stems.items():
        stem_peak = np.max(np.abs(stem))
        if stem_peak > 0:
            stem = stem * (0.82 / stem_peak)
        sf.write(AUDIO_DIR / name, stem.astype(np.float32), SR, subtype="PCM_24")
    sf.write(AUDIO_DIR / "purify-signal-score.wav", mix.astype(np.float32), SR, subtype="PCM_24")
    print(AUDIO_DIR / "purify-signal-score.wav")


if __name__ == "__main__":
    main()
