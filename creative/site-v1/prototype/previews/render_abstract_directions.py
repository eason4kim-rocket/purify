from io import BytesIO
from pathlib import Path

import cairosvg
from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parents[2]
ASSET_DIR = ROOT / "assets" / "abstract"
OUTPUT_PATH = Path(__file__).resolve().parent / "abstract-directions-v1.png"

WHITE = "#fcfcf9"
INK = "#0a1519"
INK_SOFT = "#415057"
HAIRLINE = "#dfe4e2"
BLUE = "#236881"
UI_FONT = "/System/Library/Fonts/HelveticaNeue.ttc"


def font(size: int) -> ImageFont.FreeTypeFont:
    return ImageFont.truetype(UI_FONT, size=size)


def render_svg(path: Path, width: int, height: int) -> Image.Image:
    data = cairosvg.svg2png(
        url=str(path),
        output_width=width,
        output_height=height,
        background_color=WHITE,
    )
    return Image.open(BytesIO(data)).convert("RGB")


directions = [
    ("01", "Dense field", "memory-field-graphic-v1.svg"),
    ("02", "Open matrix", "memory-field-graphic-v2.svg"),
    ("03", "Modular weave", "memory-field-graphic-v3.svg"),
]

canvas = Image.new("RGB", (1920, 610), WHITE)
draw = ImageDraw.Draw(canvas)
draw.text((64, 54), "PURIFY / ABSTRACT FIELD STUDIES", fill=INK, font=font(24), anchor="lm")
draw.text((64, 88), "One square vocabulary, three levels of density.", fill=INK_SOFT, font=font(16), anchor="lm")

card_width = 560
card_height = 311
gap = 56
start_x = 64
top = 148

for index, (number, label, filename) in enumerate(directions):
    x = start_x + index * (card_width + gap)
    art = render_svg(ASSET_DIR / filename, card_width, card_height)
    canvas.paste(art, (x, top))
    draw.rectangle((x, top, x + card_width, top + card_height), outline=HAIRLINE, width=1)
    draw.text((x, top + card_height + 38), number, fill=BLUE, font=font(14), anchor="lm")
    draw.text((x + 40, top + card_height + 38), label, fill=INK, font=font(17), anchor="lm")
    if index == 1:
        draw.rounded_rectangle(
            (x + card_width - 142, top + card_height + 20, x + card_width, top + card_height + 54),
            radius=17,
            fill=INK,
        )
        draw.text(
            (x + card_width - 71, top + card_height + 37),
            "RECOMMENDED",
            fill=WHITE,
            font=font(11),
            anchor="mm",
        )

canvas.save(OUTPUT_PATH, optimize=True)
