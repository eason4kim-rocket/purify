#!/usr/bin/env python3
"""Render the first 1080p Purify signal-film cut.

The renderer turns approved style frames into a precisely timed 36-second
animatic, adds tactile signal animation, and muxes the original score. It also
exports a muted web-background derivative with a quiet left copy-safe veil.
"""

from __future__ import annotations

import math
import subprocess
from pathlib import Path

import cv2
import imageio_ffmpeg
import numpy as np
from PIL import Image


W = 1920
H = 1080
FPS = 30
BPM = 100
BEAT_FRAMES = int(FPS * 60 / BPM)  # 18
BAR_FRAMES = 4 * BEAT_FRAMES  # 72
BARS = 15
TOTAL_FRAMES = BAR_FRAMES * BARS  # 1080

ROOT = Path(__file__).resolve().parents[1]
STILLS = ROOT / "assets" / "stills"
AUDIO = ROOT / "assets" / "audio" / "purify-signal-score.wav"
OUTPUT = ROOT / "output"
MASTER = OUTPUT / "purify-signal-film-1080p.mp4"
REVIEW = OUTPUT / "purify-signal-film-review-1080p.mp4"
WEB = OUTPUT / "purify-signal-film-web-bg-1080p.mp4"
CONTACT = OUTPUT / "purify-signal-storyboard.png"
VEIL = OUTPUT / "web-copy-safe-veil.png"
FFMPEG = imageio_ffmpeg.get_ffmpeg_exe()


def smoothstep(value: float) -> float:
    value = float(np.clip(value, 0.0, 1.0))
    return value * value * (3.0 - 2.0 * value)


def ease_out(value: float) -> float:
    value = float(np.clip(value, 0.0, 1.0))
    return 1.0 - (1.0 - value) ** 3


def load_rgb(name: str) -> np.ndarray:
    image = cv2.imread(str(STILLS / name), cv2.IMREAD_COLOR)
    if image is None:
        raise FileNotFoundError(STILLS / name)
    return cv2.cvtColor(image, cv2.COLOR_BGR2RGB)


IMAGES = {
    "whale": load_rgb("01-whale-tail.png"),
    "dish": load_rgb("02-radio-dish.png"),
    "sensor": load_rgb("03-biosensor.png"),
    "deck": load_rgb("04-hydrophone-deck.png"),
    "underwater": load_rgb("05-hydrophone-underwater.png"),
    "listener": load_rgb("06-listener.png"),
    "films": load_rgb("10-signal-films.png"),
    "researchers": load_rgb("14-researchers.png"),
    "ending": load_rgb("15-ocean-ending.png"),
}


def cover_resize(image: np.ndarray, width: int = W, height: int = H) -> np.ndarray:
    ih, iw = image.shape[:2]
    scale = max(width / iw, height / ih)
    rw = max(width, int(round(iw * scale)))
    rh = max(height, int(round(ih * scale)))
    resized = cv2.resize(image, (rw, rh), interpolation=cv2.INTER_LANCZOS4)
    x = (rw - width) // 2
    y = (rh - height) // 2
    return resized[y : y + height, x : x + width]


def ken_burns(
    image: np.ndarray,
    progress: float,
    zoom0: float,
    zoom1: float,
    focus0: tuple[float, float],
    focus1: tuple[float, float],
) -> np.ndarray:
    p = ease_out(progress)
    zoom = zoom0 + (zoom1 - zoom0) * p
    focus_x = focus0[0] + (focus1[0] - focus0[0]) * p
    focus_y = focus0[1] + (focus1[1] - focus0[1]) * p
    base = cover_resize(image)
    crop_w = max(8, int(round(W / zoom)))
    crop_h = max(8, int(round(H / zoom)))
    center_x = int(round(focus_x * W))
    center_y = int(round(focus_y * H))
    x0 = int(np.clip(center_x - crop_w // 2, 0, W - crop_w))
    y0 = int(np.clip(center_y - crop_h // 2, 0, H - crop_h))
    crop = base[y0 : y0 + crop_h, x0 : x0 + crop_w]
    return cv2.resize(crop, (W, H), interpolation=cv2.INTER_LANCZOS4)


def crop_signal_film(kind: str) -> np.ndarray:
    source = IMAGES["films"]
    ih, iw = source.shape[:2]
    boxes = {
        "ocean": (0.40, 0.04, 0.89, 0.43),
        "life": (0.43, 0.30, 0.91, 0.66),
        "sky": (0.37, 0.53, 0.87, 0.93),
    }
    x0, y0, x1, y1 = boxes[kind]
    crop = source[int(y0 * ih) : int(y1 * ih), int(x0 * iw) : int(x1 * iw)]
    # Keep the physical film's imperfect fibers, then let procedural signal
    # details restore sharpness after the macro enlargement.
    return cover_resize(crop)


FILM_MACROS = {kind: crop_signal_film(kind) for kind in ("ocean", "life", "sky")}


def add_glass_sweep(frame: np.ndarray, progress: float, strength: float = 0.12) -> np.ndarray:
    x = int((-0.30 + 1.60 * progress) * W)
    xs = np.arange(W)
    band = np.exp(-((xs - x) / (0.13 * W)) ** 2) * strength
    overlay = np.repeat(band[None, :, None], H, axis=0)
    return np.clip(frame.astype(np.float32) + overlay * 255.0, 0, 255).astype(np.uint8)


def draw_life_signal(frame: np.ndarray, progress: float) -> np.ndarray:
    overlay = np.zeros_like(frame)
    limit = int(W * (0.22 + 0.72 * ease_out(progress)))
    x = np.arange(0, max(2, limit))
    baseline = int(H * 0.54)
    y = baseline + 18.0 * np.sin(x / 95.0)
    for center in (0.35 * W, 0.66 * W):
        y += -185.0 * np.exp(-((x - center) / 18.0) ** 2)
        y += 85.0 * np.exp(-((x - (center + 30.0)) / 29.0) ** 2)
    points = np.column_stack((x, np.clip(y, 0, H - 1).astype(np.int32))).astype(np.int32)
    if len(points) > 1:
        cv2.polylines(overlay, [points], False, (45, 78, 83), 4, cv2.LINE_AA)
        cv2.polylines(overlay, [points], False, (137, 190, 190), 1, cv2.LINE_AA)
    return cv2.addWeighted(frame, 1.0, overlay, 0.48, 0.0)


def draw_pulsar(frame: np.ndarray, progress: float) -> np.ndarray:
    overlay = np.zeros_like(frame)
    rng = np.random.default_rng(8809)
    columns = np.linspace(int(W * 0.28), int(W * 0.92), 20).astype(int)
    reveal = int(len(columns) * (0.18 + 0.82 * ease_out(progress)))
    for i, x in enumerate(columns[:reveal]):
        cadence = 0.48 + 0.52 * math.sin(i * 1.71 + progress * math.tau) ** 2
        for row in range(6):
            y = int(H * (0.26 + row * 0.10) + rng.normal(0, 9))
            radius = 2 + int(4 * cadence * (1.0 if row == 2 else 0.55))
            cv2.circle(overlay, (x, y), radius, (61, 91, 97), -1, cv2.LINE_AA)
        cv2.line(overlay, (x, int(H * 0.22)), (x, int(H * 0.80)), (80, 102, 103), 1, cv2.LINE_AA)
    return cv2.addWeighted(frame, 1.0, overlay, 0.42, 0.0)


def data_macro(kind: str, progress: float) -> np.ndarray:
    frame = ken_burns(FILM_MACROS[kind], progress, 1.015, 1.055, (0.60, 0.50), (0.64, 0.50))
    if kind == "ocean":
        # A slow moving light makes the existing physical spectrogram breathe.
        frame = add_glass_sweep(frame, progress, 0.10)
    elif kind == "life":
        frame = draw_life_signal(frame, progress)
    else:
        frame = draw_pulsar(frame, progress)
    return frame


def add_underwater_particles(frame: np.ndarray, progress: float) -> np.ndarray:
    overlay = frame.copy()
    rng = np.random.default_rng(5005)
    for _ in range(52):
        x = int(rng.uniform(0.03, 0.98) * W)
        y0 = rng.uniform(0.05, 0.98) * H
        speed = rng.uniform(15.0, 80.0)
        y = int((y0 + speed * progress) % H)
        radius = 1 if rng.random() < 0.82 else 2
        alpha = rng.uniform(0.18, 0.55)
        color = int(210 + 35 * alpha)
        cv2.circle(overlay, (x, y), radius, (color, color, color), -1, cv2.LINE_AA)
    return cv2.addWeighted(frame, 0.93, overlay, 0.07, 0.0)


def add_missing_band(frame: np.ndarray, progress: float) -> np.ndarray:
    result = frame.copy()
    # The hand shifts the comparison, then motion stops during the missing beat.
    p = min(progress / 0.55, 1.0)
    shift = int(round(18.0 * math.sin(p * math.pi / 2.0)))
    result = np.roll(result, shift, axis=1)
    band_x0 = int(W * (0.62 + 0.015 * math.sin(progress * math.pi)))
    band_x1 = band_x0 + int(W * 0.065)
    soft = result[:, band_x0:band_x1].astype(np.float32)
    ivory = np.array([233.0, 228.0, 215.0], dtype=np.float32)
    result[:, band_x0:band_x1] = np.clip(soft * 0.30 + ivory * 0.70, 0, 255).astype(np.uint8)
    return result


def add_whale_breath(frame: np.ndarray, progress: float) -> np.ndarray:
    if not 0.12 <= progress <= 0.80:
        return frame
    overlay = frame.copy()
    phase = (progress - 0.12) / 0.68
    center_x = int(W * 0.605)
    center_y = int(H * (0.56 - 0.09 * phase))
    for i in range(7):
        spread = int((22 + 95 * phase) * (0.62 + i * 0.08))
        alpha = max(0.0, (1.0 - phase) * (0.18 - i * 0.014))
        cv2.ellipse(
            overlay,
            (center_x + i * 4, center_y - i * 8),
            (max(5, spread), max(4, int(spread * 0.42))),
            -86,
            0,
            360,
            (248, 248, 244),
            -1,
            cv2.LINE_AA,
        )
        frame = cv2.addWeighted(frame, 1.0, overlay, alpha, 0.0)
    return frame


def shot_frame(shot: int, progress: float) -> np.ndarray:
    if shot == 0:
        return ken_burns(IMAGES["whale"], progress, 1.035, 1.005, (0.64, 0.54), (0.62, 0.53))
    if shot == 1:
        return ken_burns(IMAGES["dish"], progress, 1.005, 1.045, (0.66, 0.50), (0.69, 0.47))
    if shot == 2:
        return ken_burns(IMAGES["sensor"], progress, 1.025, 1.065, (0.70, 0.53), (0.73, 0.52))
    if shot == 3:
        return ken_burns(IMAGES["deck"], progress, 1.015, 1.060, (0.69, 0.48), (0.72, 0.58))
    if shot == 4:
        base = ken_burns(IMAGES["underwater"], progress, 1.010, 1.050, (0.69, 0.43), (0.69, 0.58))
        return add_underwater_particles(base, progress)
    if shot == 5:
        return ken_burns(IMAGES["listener"], progress, 1.020, 1.065, (0.68, 0.48), (0.72, 0.47))
    if shot == 6:
        return data_macro("ocean", progress)
    if shot == 7:
        return data_macro("life", progress)
    if shot == 8:
        return data_macro("sky", progress)
    if shot == 9:
        return ken_burns(IMAGES["films"], progress, 1.055, 1.005, (0.66, 0.50), (0.61, 0.50))
    if shot == 10:
        base = ken_burns(IMAGES["films"], min(progress, 0.70), 1.005, 1.035, (0.62, 0.50), (0.66, 0.50))
        return add_missing_band(base, progress)
    if shot == 11:
        return ken_burns(IMAGES["sensor"], progress, 1.080, 1.120, (0.73, 0.52), (0.75, 0.51))
    if shot == 12:
        return ken_burns(IMAGES["dish"], progress, 1.075, 1.125, (0.72, 0.68), (0.74, 0.64))
    if shot == 13:
        return ken_burns(IMAGES["researchers"], progress, 1.010, 1.045, (0.70, 0.57), (0.73, 0.56))
    base = ken_burns(IMAGES["ending"], progress, 1.040, 1.000, (0.67, 0.55), (0.65, 0.54))
    return add_whale_breath(base, progress)


VIGNETTE_X = np.linspace(-1.0, 1.0, W)
VIGNETTE_Y = np.linspace(-1.0, 1.0, H)
VIGNETTE = 1.0 - 0.055 * np.clip(
    (VIGNETTE_X[None, :] ** 2 + VIGNETTE_Y[:, None] ** 2 - 0.52) / 1.48,
    0.0,
    1.0,
)


def finish_frame(frame: np.ndarray, frame_index: int) -> np.ndarray:
    work = frame.astype(np.float32)
    work *= VIGNETTE[:, :, None]
    # Deterministic, fine 35mm-like grain generated at quarter resolution.
    rng = np.random.default_rng(90_000 + frame_index)
    noise = rng.normal(0.0, 1.0, (H // 4, W // 4)).astype(np.float32)
    noise = cv2.resize(noise, (W, H), interpolation=cv2.INTER_CUBIC)
    luma = cv2.cvtColor(np.clip(work, 0, 255).astype(np.uint8), cv2.COLOR_RGB2GRAY).astype(np.float32)
    grain_gain = 2.0 + 1.8 * (1.0 - luma / 255.0)
    work += noise[:, :, None] * grain_gain[:, :, None]

    # Tiny exposure movement on each beat; perceptible as life, not flashing.
    beat_phase = (frame_index % BEAT_FRAMES) / BEAT_FRAMES
    work *= 1.0 + 0.004 * math.exp(-beat_phase * 8.0)

    # Blend from and to the white website field.
    if frame_index < 12:
        alpha = smoothstep(frame_index / 11.0)
        work = 255.0 * (1.0 - alpha) + work * alpha
    if frame_index >= TOTAL_FRAMES - 12:
        alpha = smoothstep((frame_index - (TOTAL_FRAMES - 12)) / 11.0)
        work = work * (1.0 - alpha) + 255.0 * alpha
    return np.clip(work, 0, 255).astype(np.uint8)


def render_frame(frame_index: int) -> np.ndarray:
    shot = min(BARS - 1, frame_index // BAR_FRAMES)
    within = frame_index % BAR_FRAMES
    progress = within / max(1, BAR_FRAMES - 1)
    return finish_frame(shot_frame(shot, progress), frame_index)


def save_contact_sheet() -> None:
    thumb_w, thumb_h = 384, 216
    sheet = np.full((thumb_h * 3, thumb_w * 5, 3), 248, dtype=np.uint8)
    for shot in range(BARS):
        frame_index = shot * BAR_FRAMES + BAR_FRAMES // 2
        thumb = cv2.resize(render_frame(frame_index), (thumb_w, thumb_h), interpolation=cv2.INTER_AREA)
        row, col = divmod(shot, 5)
        sheet[row * thumb_h : (row + 1) * thumb_h, col * thumb_w : (col + 1) * thumb_w] = thumb
    Image.fromarray(sheet).save(CONTACT, quality=94)


def save_veil() -> None:
    rgba = np.zeros((H, W, 4), dtype=np.uint8)
    rgba[:, :, :3] = 255
    x = np.arange(W, dtype=np.float32)
    alpha = np.clip(0.52 * (1.0 - x / (W * 0.56)), 0.0, 0.52)
    alpha *= 1.0 - 0.15 * np.sin(np.clip(x / (W * 0.56), 0.0, 1.0) * np.pi)
    rgba[:, :, 3] = np.round(alpha * 255.0).astype(np.uint8)[None, :]
    Image.fromarray(rgba, "RGBA").save(VEIL)


def render_master() -> None:
    OUTPUT.mkdir(parents=True, exist_ok=True)
    command = [
        FFMPEG,
        "-y",
        "-hide_banner",
        "-loglevel",
        "warning",
        "-f",
        "rawvideo",
        "-pix_fmt",
        "rgb24",
        "-s",
        f"{W}x{H}",
        "-r",
        str(FPS),
        "-i",
        "-",
        "-i",
        str(AUDIO),
        "-map",
        "0:v:0",
        "-map",
        "1:a:0",
        "-c:v",
        "libx264",
        "-preset",
        "medium",
        "-crf",
        "18",
        "-pix_fmt",
        "yuv420p",
        "-c:a",
        "aac",
        "-b:a",
        "256k",
        "-ar",
        "48000",
        "-t",
        "36.0",
        "-movflags",
        "+faststart",
        str(MASTER),
    ]
    process = subprocess.Popen(command, stdin=subprocess.PIPE)
    assert process.stdin is not None
    try:
        for frame_index in range(TOTAL_FRAMES):
            process.stdin.write(render_frame(frame_index).tobytes())
            if frame_index % 180 == 179:
                print(f"rendered {frame_index + 1}/{TOTAL_FRAMES} frames", flush=True)
    finally:
        process.stdin.close()
    return_code = process.wait()
    if return_code:
        raise RuntimeError(f"ffmpeg master render failed: {return_code}")


def render_web_version() -> None:
    command = [
        FFMPEG,
        "-y",
        "-hide_banner",
        "-loglevel",
        "warning",
        "-i",
        str(MASTER),
        "-loop",
        "1",
        "-i",
        str(VEIL),
        "-filter_complex",
        "[0:v][1:v]overlay=0:0:shortest=1,eq=saturation=0.88:contrast=0.95:brightness=0.018[v]",
        "-map",
        "[v]",
        "-an",
        "-c:v",
        "libx264",
        "-preset",
        "slow",
        "-crf",
        "25",
        "-pix_fmt",
        "yuv420p",
        "-t",
        "36.0",
        "-movflags",
        "+faststart",
        str(WEB),
    ]
    subprocess.run(command, check=True)


def render_review_version() -> None:
    command = [
        FFMPEG,
        "-y",
        "-hide_banner",
        "-loglevel",
        "warning",
        "-i",
        str(MASTER),
        "-map",
        "0:v:0",
        "-map",
        "0:a:0",
        "-c:v",
        "libx264",
        "-preset",
        "slow",
        "-crf",
        "22",
        "-pix_fmt",
        "yuv420p",
        "-c:a",
        "copy",
        "-movflags",
        "+faststart",
        str(REVIEW),
    ]
    subprocess.run(command, check=True)


def main() -> None:
    if not AUDIO.exists():
        raise FileNotFoundError(f"Compose the score first: {AUDIO}")
    save_contact_sheet()
    save_veil()
    render_master()
    render_review_version()
    render_web_version()
    print(MASTER)
    print(REVIEW)
    print(WEB)
    print(CONTACT)


if __name__ == "__main__":
    main()
