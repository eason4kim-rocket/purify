#!/usr/bin/env python3
"""Extract deterministic point targets from the approved human-memory source image."""

from __future__ import annotations

import argparse
from pathlib import Path

import numpy as np
from PIL import Image


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--count", type=int, default=2600)
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    source = Image.open(args.source).convert("RGB").resize((360, 450), Image.Resampling.LANCZOS)
    rgb = np.asarray(source, dtype=np.float32)
    luminance = (rgb[..., 0] * 0.2126 + rgb[..., 1] * 0.7152 + rgb[..., 2] * 0.0722) / 255.0
    ink = np.clip((0.985 - luminance) / 0.985, 0.0, 1.0)
    gradient_y, gradient_x = np.gradient(ink)
    edge = np.hypot(gradient_x, gradient_y)
    edge /= max(float(edge.max()), 1e-6)

    grid_y, grid_x = np.mgrid[1:449:2, 1:359:2]
    candidate_ink = ink[grid_y, grid_x]
    candidate_edge = edge[grid_y, grid_x]
    mask = (candidate_ink > 0.025) | (candidate_edge > 0.045)
    xs = grid_x[mask]
    ys = grid_y[mask]
    inks = candidate_ink[mask]
    edges = candidate_edge[mask]

    weights = 0.58 * np.power(np.maximum(inks, 0.002), 0.72) + 0.42 * np.power(np.maximum(edges, 0.002), 0.58)
    rng = np.random.default_rng(20260809)
    count = min(args.count, len(xs))
    selected = rng.choice(len(xs), size=count, replace=False, p=weights / weights.sum())

    points: list[list[float | int]] = []
    for index in selected:
      strength = float(np.clip(inks[index] * 0.72 + edges[index] * 0.62, 0.0, 1.0))
      is_edge = int(edges[index] > 0.12)
      size = 1.15 + strength * 1.85 + is_edge * 0.35
      opacity = 0.38 + strength * 0.55
      points.append([
          round((float(xs[index]) + 0.5) / 360.0, 5),
          round((float(ys[index]) + 0.5) / 450.0, 5),
          round(size, 2),
          round(opacity, 2),
          is_edge,
      ])

    points.sort(key=lambda point: (point[1], point[0]))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    payload = "window.PURIFY_MEMORY_POINTS=" + repr(points).replace(" ", "") + ";\n"
    args.output.write_text(payload, encoding="utf-8")
    print(f"wrote {len(points)} point targets to {args.output}")


if __name__ == "__main__":
    main()
