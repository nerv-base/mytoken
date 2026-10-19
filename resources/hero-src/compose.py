#!/usr/bin/env python3
"""Compose the README hero images (docs/hero.png, docs/hero-zh.png).

Inputs are window captures of the demo build (see CONTRIBUTING.md, "Demo mode
and README images"): <shots>/<lang>-overview.png and <shots>/<lang>-tray.png,
taken at 2x with `screencapture -o -l <window>` so their corners are clear.

    python3 resources/hero-src/compose.py /tmp/demoshots

Needs Pillow and macOS's SF Pro and PingFang fonts.
"""
import glob
import sys

from PIL import Image, ImageDraw, ImageFilter, ImageFont

W, H = 2400, 1100
SF = "/System/Library/Fonts/SFNS.ttf"
PINGFANG = (glob.glob("/System/Library/AssetsV2/com_apple_MobileAsset_Font*/*/AssetData/PingFang.ttc") or [SF])[0]

# The orb's light: periwinkle, violet, rose.
PERI, VIOLET, ROSE = (140, 158, 255), (185, 124, 240), (229, 138, 214)

TEXT = {
    "en": {
        "tagline": "Where did all your tokens go?",
        "sub": ["Spend, plan quotas and live sessions for 32 AI",
                "coding tools — native, local, private."],
        "chips": ["Claude Code", "Codex", "Gemini CLI", "OpenCode", "+28 more"],
    },
    "zh": {
        "tagline": "你的 token 都去哪儿了？",
        "sub": ["32 个 AI 编程工具的花费、套餐额度",
                "和正在进行的会话——原生、本地、私密。"],
        "chips": ["Claude Code", "Codex", "Gemini CLI", "OpenCode", "等 28 个"],
    },
}


def font(size, weight="Bold", cjk=False):
    if cjk:
        index = {"Bold": 5, "Semibold": 4, "Medium": 3, "Regular": 2}.get(weight, 2)
        try:
            return ImageFont.truetype(PINGFANG, size, index=index)
        except OSError:
            pass
    f = ImageFont.truetype(SF, size)
    f.set_variation_by_name(weight)
    return f


def glow(img, xy, radius, color, alpha):
    layer = Image.new("RGBA", img.size, (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)
    x, y = xy
    d.ellipse((x - radius, y - radius, x + radius, y + radius), fill=color + (alpha,))
    layer = layer.filter(ImageFilter.GaussianBlur(radius * 0.55))
    img.alpha_composite(layer)


def shadowed(img, shot, xy, blur=40, offset=24, alpha=150):
    sh = Image.new("RGBA", img.size, (0, 0, 0, 0))
    mask = shot.split()[3].point(lambda a: alpha if a > 0 else 0)
    sh.paste((0, 0, 0, 255), (xy[0], xy[1] + offset), mask)
    img.alpha_composite(sh.filter(ImageFilter.GaussianBlur(blur)))
    img.alpha_composite(shot, xy)


def gradient_text(img, xy, text, f, stops):
    mask = Image.new("L", img.size, 0)
    ImageDraw.Draw(mask).text(xy, text, font=f, fill=255)
    box = mask.getbbox()
    grad = Image.new("RGBA", img.size, (0, 0, 0, 0))
    gd = ImageDraw.Draw(grad)
    x0, x1 = box[0], box[2]
    for x in range(x0, x1 + 1):
        t = (x - x0) / max(1, x1 - x0) * (len(stops) - 1)
        i = min(int(t), len(stops) - 2)
        k = t - i
        c = tuple(int(stops[i][j] * (1 - k) + stops[i + 1][j] * k) for j in range(3))
        gd.line([(x, 0), (x, img.size[1])], fill=c + (255,))
    img.paste(grad, (0, 0), mask)
    return box


def compose(shots, lang, out):
    t = TEXT[lang]
    cjk = lang == "zh"
    img = Image.new("RGBA", (W, H), (14, 14, 18, 255))
    glow(img, (1500, 80), 620, PERI, 70)
    glow(img, (180, 1050), 520, ROSE, 55)
    glow(img, (2300, 900), 420, VIOLET, 50)

    # The product: the overview window running off the right edge, the
    # menu-bar panel floating over its corner.
    win = Image.open(f"{shots}/{lang}-overview.png").convert("RGBA")
    win = win.resize((1560, int(win.height * 1560 / win.width)), Image.LANCZOS)
    shadowed(img, win, (1100, 150))
    panel = Image.open(f"{shots}/{lang}-tray.png").convert("RGBA")
    panel = panel.resize((int(panel.width * 0.72), int(panel.height * 0.72)), Image.LANCZOS)
    shadowed(img, panel, (1010, 330), blur=50, offset=30, alpha=190)

    d = ImageDraw.Draw(img)
    x = 120
    icon = Image.open("resources/icon.png").convert("RGBA").resize((168, 168), Image.LANCZOS)
    img.alpha_composite(icon, (x - 14, 150))

    title = font(120, "Bold")
    d.text((x, 340), "MyToken", font=title, fill=(255, 255, 255, 255))
    tw = d.textlength("MyToken", font=title)
    gradient_text(img, (x + tw + 6, 340), "!!!!!", title, [PERI, VIOLET, ROSE])

    d.text((x, 530), t["tagline"], font=font(52, "Semibold", cjk), fill=(245, 245, 250, 255))
    sub = font(34, "Regular", cjk)
    for i, line in enumerate(t["sub"]):
        d.text((x, 620 + i * 52), line, font=sub, fill=(170, 172, 186, 255))

    chip = font(26, "Medium", cjk)
    layer = Image.new("RGBA", img.size, (0, 0, 0, 0))
    ld = ImageDraw.Draw(layer)
    cx, cy = x, 770
    for name in t["chips"]:
        w = ld.textlength(name, font=chip)
        ld.rounded_rectangle((cx, cy, cx + w + 40, cy + 52), radius=26,
                             fill=(255, 255, 255, 18), outline=(255, 255, 255, 50), width=2)
        ld.text((cx + 20, cy + 10), name, font=chip, fill=(225, 226, 235, 255))
        cx += w + 40 + 12
    img.alpha_composite(layer)

    img.convert("RGB").save(out, optimize=True)


if __name__ == "__main__":
    shots = sys.argv[1] if len(sys.argv) > 1 else "/tmp/demoshots"
    compose(shots, "en", "docs/hero.png")
    compose(shots, "zh", "docs/hero-zh.png")
