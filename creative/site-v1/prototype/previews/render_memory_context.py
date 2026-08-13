from pathlib import Path

from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parents[3]
PREVIEW_DIR = Path(__file__).resolve().parent
ART_PATH = PREVIEW_DIR / "memory-field-graphic-v2.png"
OUTPUT_PATH = PREVIEW_DIR / "memory-section-context-v2.png"

CANVAS = (1600, 1440)
WHITE = "#fcfcf9"
INK = "#0a1519"
INK_SOFT = "#415057"
INK_FAINT = "#758187"
BLUE = "#2d87a7"

DISPLAY_FONT = "/System/Library/Fonts/Supplemental/Iowan Old Style.ttc"
UI_FONT = "/System/Library/Fonts/HelveticaNeue.ttc"


def font(path: str, size: int) -> ImageFont.FreeTypeFont:
    return ImageFont.truetype(path, size=size)


canvas = Image.new("RGB", CANVAS, WHITE)
draw = ImageDraw.Draw(canvas)

draw.text((80, 82), "02", fill=INK_FAINT, font=font(UI_FONT, 13), anchor="lm")
draw.text((126, 82), "LIVING MEMORY", fill=INK_FAINT, font=font(UI_FONT, 13), anchor="lm")

draw.multiline_text(
    (800, 220),
    "The web is humanity’s\nliving memory.",
    fill=INK,
    font=font(DISPLAY_FONT, 78),
    anchor="mm",
    align="center",
    spacing=-2,
)
draw.text(
    (800, 346),
    "Our questions, discoveries, mistakes and ideas—still changing, still connected.",
    fill=INK_SOFT,
    font=font(UI_FONT, 22),
    anchor="mm",
)

draw.multiline_text(
    (800, 486),
    "The next intelligence should inherit more than information.\nIt should inherit the context that gives it meaning.",
    fill=INK,
    font=font(DISPLAY_FONT, 42),
    anchor="mm",
    align="center",
    spacing=2,
)
draw.text(
    (800, 578),
    "BLUE FIELD / FRAGMENT · OVERLAP · RECONNECT",
    fill=BLUE,
    font=font(UI_FONT, 12),
    anchor="mm",
)

art = Image.open(ART_PATH).convert("RGB")
if art.size != (1440, 800):
    art = art.resize((1440, 800), Image.Resampling.LANCZOS)
canvas.paste(art, (80, 640))

canvas.save(OUTPUT_PATH, optimize=True)
