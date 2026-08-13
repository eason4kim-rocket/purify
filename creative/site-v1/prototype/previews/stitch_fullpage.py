#!/usr/bin/env python3
"""Stitch viewport screenshots into one page while removing the fixed header overlap."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from PIL import Image


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--height", type=int, required=True)
    parser.add_argument("--header-crop", type=int, default=100)
    args = parser.parse_args()

    segments = json.loads(args.manifest.read_text())
    if not segments:
        raise SystemExit("No screenshot segments supplied")

    first = Image.open(segments[0]["path"]).convert("RGB")
    canvas = Image.new("RGB", (first.width, args.height), "#fcfcf9")
    covered_until = 0

    for index, segment in enumerate(segments):
        image = Image.open(segment["path"]).convert("RGB")
        page_y = int(segment["y"])
        crop_top = 0 if index == 0 else args.header_crop
        paste_y = page_y + crop_top

        if paste_y < covered_until:
            crop_top += covered_until - paste_y
            paste_y = covered_until

        available_height = min(image.height - crop_top, args.height - paste_y)
        if available_height <= 0:
            continue

        cropped = image.crop((0, crop_top, image.width, crop_top + available_height))
        canvas.paste(cropped, (0, paste_y))
        covered_until = max(covered_until, paste_y + available_height)

    args.output.parent.mkdir(parents=True, exist_ok=True)
    canvas.save(args.output, optimize=True)


if __name__ == "__main__":
    main()
