# -*- coding: utf-8 -*-
"""生成 App 图标的所有位图资源。

设计基调（与手机界面同一套配色）：
    底色 #0F1115 附近  —— 与 web/index.html 的 --bg 一致
    主色 #5FD3A0 薄荷绿 —— 与界面里的 --accent 一致（原图标用的是蓝色，和界面不统一）

所有绘制都在 4 倍画布上做，最后 LANCZOS 缩小，边缘自然抗锯齿。
"""

import math
import os

from PIL import Image, ImageDraw, ImageFilter
from PIL import ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
PREVIEW = os.path.join(HERE, "preview")

S = 2048          # 超采样画布边长
OUT = 512         # 成品边长

BG_TOP = (26, 31, 40)
BG_BOT = (11, 13, 17)
MINT = (95, 211, 160)
MINT_HI = (130, 227, 184)     # 图标底色（上）
MINT_LO = (69, 190, 139)      # 图标底色（下）


# --------------------------------------------------------------------------
# 基础绘制工具
# --------------------------------------------------------------------------

def vgrad(size, top, bottom):
    img = Image.new("RGB", (size, size), bottom)
    d = ImageDraw.Draw(img)
    for y in range(size):
        t = y / (size - 1)
        # 轻微的缓动，让上方亮部更集中在顶部
        t = t ** 1.15
        d.line([(0, y), (size, y)], fill=(
            round(top[0] + (bottom[0] - top[0]) * t),
            round(top[1] + (bottom[1] - top[1]) * t),
            round(top[2] + (bottom[2] - top[2]) * t),
        ))
    return img


def rounded(draw, box, radius, fill=None, outline=None, width=1):
    draw.rounded_rectangle(box, radius=radius, fill=fill, outline=outline, width=width)


def caps_stroke(layer, pts, width, fill):
    """带圆头的折线。Pillow 的 joint='curve' 处理拐角，再在两端补圆点。"""
    d = ImageDraw.Draw(layer)
    d.line(pts, fill=fill, width=width, joint="curve")
    r = width / 2
    for (x, y) in (pts[0], pts[-1]):
        d.ellipse([x - r, y - r, x + r, y + r], fill=fill)


def arc_stroke(layer, box, start, end, width, fill):
    """带圆头的圆弧：先画弧，再在两端补圆点。"""
    d = ImageDraw.Draw(layer)
    d.arc(box, start=start, end=end, fill=fill, width=width)
    cx, cy = (box[0] + box[2]) / 2, (box[1] + box[3]) / 2
    rx, ry = (box[2] - box[0]) / 2, (box[3] - box[1]) / 2
    r = width / 2
    for ang in (start, end):
        a = math.radians(ang)
        x, y = cx + rx * math.cos(a), cy + ry * math.sin(a)
        d.ellipse([x - r, y - r, x + r, y + r], fill=fill)


def center_layer(layer, cx, cy, box=None):
    """把图层内容整体平移到画布中心附近。"""
    bbox = box or layer.getbbox()
    if not bbox:
        return layer
    dx = cx - (bbox[0] + bbox[2]) / 2
    dy = cy - (bbox[1] + bbox[3]) / 2
    return layer.transform(layer.size, Image.AFFINE, (1, 0, -dx, 0, 1, -dy),
                           resample=Image.BICUBIC)


def compose(base, glyph, glow=0, glow_alpha=0.55):
    if glow:
        halo = glyph.filter(ImageFilter.GaussianBlur(glow))
        a = halo.getchannel("A").point(lambda v: round(v * glow_alpha))
        halo.putalpha(a)
        base = Image.alpha_composite(base.convert("RGBA"), halo)
    return Image.alpha_composite(base.convert("RGBA"), glyph)


def rounded_square_mask(size, radius_ratio=0.225):
    m = Image.new("L", (size, size), 0)
    ImageDraw.Draw(m).rounded_rectangle([0, 0, size - 1, size - 1],
                                        radius=int(size * radius_ratio), fill=255)
    return m


def downscale(img, out):
    return img.convert("RGBA").resize((out, out), Image.LANCZOS)


# --------------------------------------------------------------------------
# 提示符（>_）这一组几何是所有方案共用的
# --------------------------------------------------------------------------

CHEV = [(0, 0), (520, 386), (0, 772)]      # 相对坐标
UNDER = (668, 646, 1024, 778)              # x0, y0, x1, y1
STROKE = 132


def draw_prompt(layer, scale=1.0, ox=0, oy=0, color=MINT, stroke=None,
                chev=None, under=None):
    sw = int((stroke or STROKE) * scale)
    ch = chev or CHEV
    un = under or UNDER
    pts = [(ox + x * scale, oy + y * scale) for (x, y) in ch]
    caps_stroke(layer, pts, sw, color)
    x0, y0, x1, y1 = un
    rounded(ImageDraw.Draw(layer),
            [ox + x0 * scale, oy + y0 * scale, ox + x1 * scale, oy + y1 * scale],
            radius=sw / 2, fill=color)


def prompt_layer(scale=1.0, color=MINT, stroke=None):
    layer = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(layer, S, scale=scale, color=color, stroke=stroke)
    return layer


def draw_centered_prompt(layer, size, scale=1.0, color=MINT, stroke=None,
                         dx=0, dy=0):
    """直接把提示符画到目标位置上。

    注意不能"先画在原点再整体平移"——那样左上角会被画布裁掉，圆头就没了。
    """
    g = prompt_geometry(scale)
    if stroke:
        g["stroke"] = stroke * scale      # 传进来的线宽按未缩放的设计单位算
    bbox = prompt_bbox(g)
    k = size / S
    ox = size / 2 - (bbox[0] + bbox[2]) / 2 * k + dx * k
    oy = size / 2 - (bbox[1] + bbox[3]) / 2 * k + dy * k
    sw = int(round(g["stroke"] * k))
    caps_stroke(layer, [(x * k + ox, y * k + oy) for (x, y) in g["chev"]],
                sw, color)
    (ax, ay), (bx, by) = g["underline"]
    caps_stroke(layer, [(ax * k + ox, ay * k + oy),
                        (bx * k + ox, by * k + oy)], sw, color)


# --------------------------------------------------------------------------
# 方案 A：干净提示符
# --------------------------------------------------------------------------

def concept_a():
    base = vgrad(S, BG_TOP, BG_BOT).convert("RGBA")
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(glyph, S, scale=1.34, color=MINT_HI, dy=10)
    return compose(base, glyph, glow=34, glow_alpha=0.40)


# --------------------------------------------------------------------------
# 方案 B：手机里跑着终端
# --------------------------------------------------------------------------

def concept_b():
    base = vgrad(S, BG_TOP, BG_BOT).convert("RGBA")
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(glyph)
    rounded(d, [596, 380, 1452, 1668], radius=230, outline=MINT_HI, width=112)
    draw_prompt(glyph, scale=0.80, ox=760, oy=700, color=MINT_HI, stroke=120)
    glyph = center_layer(glyph, S / 2, S / 2)
    return compose(base, glyph, glow=30, glow_alpha=0.35)


# --------------------------------------------------------------------------
# 方案 C：提示符 + 未读圆点（"它有事会叫你"）
# --------------------------------------------------------------------------

def concept_c():
    base = vgrad(S, BG_TOP, BG_BOT).convert("RGBA")
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(glyph, S, scale=1.22, color=MINT_HI, dx=-70, dy=40)
    # 右上角未读圆点，外面套一圈底色把它和提示符切开
    d = ImageDraw.Draw(glyph)
    cx, cy, r = 1470, 470, 168
    d.ellipse([cx - r - 62, cy - r - 62, cx + r + 62, cy + r + 62], fill=BG_TOP)
    d.ellipse([cx - r, cy - r, cx + r, cy + r], fill=MINT_HI)
    return compose(base, glyph, glow=34, glow_alpha=0.38)


# --------------------------------------------------------------------------
# 方案 D：通知气泡里的提示符
# --------------------------------------------------------------------------

def rounded_tail(w, h, radius, angle, fill):
    """单独画一个圆角长条再旋转，拼到气泡上，尾巴就不会是尖角。"""
    pad = max(w, h)
    layer = Image.new("RGBA", (pad + w * 2, pad + h * 2), (0, 0, 0, 0))
    ImageDraw.Draw(layer).rounded_rectangle(
        [pad, pad, pad + w, pad + h], radius=radius, fill=fill)
    return layer.rotate(angle, resample=Image.BICUBIC, expand=False), (pad, pad)


def concept_d():
    base = vgrad(S, BG_TOP, BG_BOT).convert("RGBA")
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(glyph)
    box = [340, 486, 1708, 1406]
    rounded(d, box, radius=226, fill=MINT_HI)
    # 圆角尾巴：从气泡左下角往外拖一小截
    tail, (tx, ty) = rounded_tail(188, 470, 94, 34, MINT_HI)
    glyph.alpha_composite(tail, (470 - tx, 1236 - ty))
    rounded(d, box, radius=226, fill=MINT_HI)   # 再盖一遍，保证主体边缘干净
    inner = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(inner, S, scale=0.92, color=BG_BOT, dy=-94)
    glyph = Image.alpha_composite(glyph, inner)
    return compose(base, glyph, glow=40, glow_alpha=0.30)


# --------------------------------------------------------------------------
# 方案 E：整块薄荷绿，提示符挖空
# --------------------------------------------------------------------------

def concept_e():
    base = Image.new("RGBA", (S, S), MINT_HI + (255,))
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(glyph, S, scale=1.30, color=(12, 15, 19, 255), dy=8)
    return Image.alpha_composite(base, glyph)


CONCEPTS = {
    "A-干净提示符": concept_a,
    "C-未读圆点": concept_c,
    "D-通知气泡": concept_d,
    "E-整块薄荷绿": concept_e,
}


# --------------------------------------------------------------------------
# 最终候选：薄荷绿实心块（干净 / 气泡 / 深底未读点）
# --------------------------------------------------------------------------

def mint_tile():
    """薄荷绿方块，带一点斜向高光，避免纯色发闷。"""
    tile = vgrad(S, MINT_HI, MINT_LO).convert("RGBA")
    sheen = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(sheen)
    for i in range(160):
        a = round(26 * (1 - i / 160) ** 1.6)
        d.line([(0, i * 6), (S, i * 6 - 700)], fill=(255, 255, 255, a), width=8)
    return Image.alpha_composite(tile, sheen)


INK = (10, 13, 17, 255)


def concept_f1():
    base = mint_tile()
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(glyph, S, scale=1.16, color=INK, dy=6)
    return Image.alpha_composite(base, glyph)


def concept_f2():
    base = mint_tile()
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    inner = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(inner, S, scale=1.10, color=INK, dx=40, dy=6)
    # 左下角的气泡尾巴，用圆角长条旋转出来
    tail, (tx, ty) = rounded_tail(190, 470, 95, 36, INK)
    glyph.alpha_composite(tail, (430 - tx, 1290 - ty))
    return Image.alpha_composite(base, Image.alpha_composite(glyph, inner))


def concept_f3():
    base = vgrad(S, BG_TOP, BG_BOT).convert("RGBA")
    glyph = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    draw_centered_prompt(glyph, S, scale=1.10, color=MINT_HI, dx=-40, dy=46)
    d = ImageDraw.Draw(glyph)
    cx, cy, r = 1430, 520, 150
    d.ellipse([cx - r - 56, cy - r - 56, cx + r + 56, cy + r + 56], fill=BG_TOP)
    d.ellipse([cx - r, cy - r, cx + r, cy + r], fill=MINT_HI)
    return compose(base, glyph, glow=30, glow_alpha=0.34)


FINALISTS = {
    "F1-薄荷块": concept_f1,
    "F2-薄荷块+气泡": concept_f2,
    "F3-深底+未读点": concept_f3,
}


def homescreen(path, name="随行"):
    """模拟手机桌面，看看图标和应用名摆在一起是什么感觉。"""
    W, H = 900, 1500
    bg = vgrad(H, (46, 52, 66), (16, 18, 24)).resize((W, H))
    sheet = bg.convert("RGBA")
    try:
        font = ImageFont.truetype("C:/Windows/Fonts/msyh.ttc", 34)
        thin = ImageFont.truetype("C:/Windows/Fonts/msyh.ttc", 26)
    except OSError:
        font = thin = ImageFont.load_default()
    d = ImageDraw.Draw(sheet)
    d.text((W / 2, 120), "9:41", font=thin, fill=(255, 255, 255, 220), anchor="mm")

    items = [("F1", concept_f1()), ("F2", concept_f2()), ("F3", concept_f3())]
    step = W // len(items)
    for i, (label, img) in enumerate(items):
        cx = step * i + step / 2
        icon = img.resize((180, 180), Image.LANCZOS)
        sheet.alpha_composite(icon, (int(cx - 90), 300))
        d.text((cx, 510), name, font=font, fill=(255, 255, 255, 235), anchor="mm")
        d.text((cx, 560), label, font=thin, fill=(255, 255, 255, 130), anchor="mm")
    sheet.convert("RGB").save(path)


# --------------------------------------------------------------------------
# 预览：并排四个方案，各尺寸各一行
# --------------------------------------------------------------------------

def contact_sheet(path):
    """四套方案 × 多种尺寸，亮底和暗底各看一遍，再看圆形遮罩下的样子。"""
    concepts = FINALISTS
    sheet_for(concepts, path)


def sheet_for(concepts, path):
    """把若干套方案排成一张对比图：亮底一排、暗底一排、外加圆形遮罩。"""
    sizes = [256, 128, 64, 32, 24]
    row_h = 256
    band_w = 700               # 右侧亮底/暗底各占一条
    pad = 28
    label_w = 230
    w = label_w + band_w * 2 + pad * 3
    h = row_h * len(concepts) + pad * (len(concepts) + 1)
    sheet = Image.new("RGBA", (w, h), (240, 241, 244, 255))
    y = pad
    for name, fn in concepts.items():
        img = downscale(fn(), 512)
        d = ImageDraw.Draw(sheet)
        d.text((pad, y + row_h / 2 - 8), name, fill=(28, 31, 36, 255))

        # 左：亮底一排
        x = label_w + pad
        for s in sizes:
            sheet.alpha_composite(img.resize((s, s), Image.LANCZOS),
                                  (x, y + (row_h - s) // 2))
            x += s + pad

        # 右：暗底一排 + 圆形遮罩
        dark_x = label_w + band_w + pad * 2
        dark = Image.new("RGBA", (band_w, row_h), (16, 18, 22, 255))
        dx = pad
        for s in sizes:
            dark.alpha_composite(img.resize((s, s), Image.LANCZOS),
                                 (dx, (row_h - s) // 2))
            dx += s + pad
        mask = Image.new("L", (192, 192), 0)
        ImageDraw.Draw(mask).ellipse([0, 0, 191, 191], fill=255)
        circ = img.resize((192, 192), Image.LANCZOS)
        circ.putalpha(mask)
        dark.alpha_composite(circ, (band_w - 192 - pad, (row_h - 192) // 2))
        sheet.alpha_composite(dark, (dark_x, y))
        y += row_h + pad
    sheet.convert("RGB").save(path)


# ==========================================================================
# 成品：薄荷绿方块 + 深色提示符
# ==========================================================================

FINAL_SCALE = 1.10
INK = (11, 15, 20, 255)        # #0B0F14
TILE_RADIUS = 0.225            # 老式图标自己带的圆角比例
ADAPTIVE = 108.0               # 自适应图标前景画布（dp）
ADAPTIVE_VIEW = 72.0           # 其中真正可见的那一块

ROOT = os.path.dirname(HERE)
WEB = os.path.join(ROOT, "codex-mobile", "web")
RES = os.path.join(ROOT, "android", "app", "app", "src", "main", "res")


def prompt_geometry(scale=FINAL_SCALE):
    """把提示符换算到设计画布坐标（画布边长 2048）。"""
    chev = [(x * scale, y * scale) for (x, y) in CHEV]
    x0, y0, x1, y1 = [v * scale for v in UNDER]
    stroke = STROKE * scale
    cy = (y0 + y1) / 2
    # 下划线画成圆头线段：两端各缩进一个圆角半径，正好还原成圆角长条
    under = ((x0 + stroke / 2, cy), (x1 - stroke / 2, cy))
    return dict(chev=chev, underline=under, stroke=stroke)


def prompt_bbox(geom):
    xs, ys = [], []
    r = geom["stroke"] / 2
    for (x, y) in list(geom["chev"]) + list(geom["underline"]):
        xs += [x - r, x + r]
        ys += [y - r, y + r]
    return (min(xs), min(ys), max(xs), max(ys))


def to_adaptive(pt, geom):
    """设计画布坐标 -> 自适应图标前景坐标（108 单位，中心 54）。"""
    bbox = prompt_bbox(geom)
    cx, cy = (bbox[0] + bbox[2]) / 2, (bbox[1] + bbox[3]) / 2
    f = ADAPTIVE_VIEW / S
    off = (ADAPTIVE - ADAPTIVE_VIEW) / 2
    return (off + (pt[0] - cx + S / 2) * f, off + (pt[1] - cy + S / 2) * f)


def radial_layer(size, cx, cy, spread, color, strength):
    """柔和的径向光斑：小图算完再放大，不会有色带。"""
    n = 128
    small = Image.new("RGBA", (n, n), (0, 0, 0, 0))
    px = small.load()
    for y in range(n):
        for x in range(n):
            d = math.hypot(x / (n - 1) - cx, y / (n - 1) - cy) / spread
            a = max(0.0, 1.0 - d) ** 2
            px[x, y] = color + (round(strength * a),)
    return small.resize((size, size), Image.BICUBIC)


def tile(size, rounded_corners=False):
    base = vgrad(size, MINT_HI, MINT_LO).convert("RGBA")
    # 左上角一片柔光，右下角轻微压暗，方块就不会显得平
    img = Image.alpha_composite(
        base, radial_layer(size, 0.28, 0.20, 1.05, (255, 255, 255), 58))
    img = Image.alpha_composite(
        img, radial_layer(size, 0.96, 1.02, 0.95, (0, 40, 26), 40))
    if rounded_corners:
        img.putalpha(rounded_square_mask(size, TILE_RADIUS))
    return img


def render_icon(target, rounded_corners=False, ss=4):
    """成品图标：整块薄荷绿 + 深色提示符。超采样后缩小，边缘干净。"""
    size = target * ss
    img = tile(size)
    glyph = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    draw_centered_prompt(glyph, size, scale=FINAL_SCALE, color=INK, dy=6)
    img = Image.alpha_composite(img, glyph)
    if rounded_corners:
        img.putalpha(rounded_square_mask(size, TILE_RADIUS))
    return img.resize((target, target), Image.LANCZOS)


def build():
    """写出两端要用的全部图标文件，并打印矢量图标要用的坐标。"""
    written = []

    # 手机网页（PWA / iOS）：满幅方块，圆角交给系统切
    for name, size in (("icon.png", 512), ("icon-192.png", 192),
                       ("icon-512.png", 512)):
        path = os.path.join(WEB, name)
        render_icon(size).save(path)
        written.append(path)

    # 托盘图标：自带圆角，免得通知区域里出现一个生硬的方块
    tray = os.path.join(WEB, "tray.png")
    render_icon(192, rounded_corners=True).save(tray)
    written.append(tray)

    # 安卓老式图标：每个密度一份，圆角已经画进去
    for folder, size in (("mipmap-mdpi", 48), ("mipmap-hdpi", 72),
                         ("mipmap-xhdpi", 96), ("mipmap-xxhdpi", 144),
                         ("mipmap-xxxhdpi", 192)):
        path = os.path.join(RES, folder, "ic_launcher.png")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        render_icon(size, rounded_corners=True).save(path)
        written.append(path)

    # 矢量版：以后要做网页 logo / 宣传图直接拿这个，不用再对位
    svg = os.path.join(HERE, "logo.svg")
    with open(svg, "w", encoding="utf-8") as f:
        f.write(logo_svg())
    written.append(svg)

    g = prompt_geometry()
    chev = [to_adaptive(p, g) for p in g["chev"]]
    under = [to_adaptive(p, g) for p in g["underline"]]
    stroke = g["stroke"] * ADAPTIVE_VIEW / S
    print()
    print("=== 自适应图标前景（108 单位画布）===")
    print("chevron  : " + " L".join("%.2f,%.2f" % p for p in chev))
    print("underline: " + " L".join("%.2f,%.2f" % p for p in under))
    print("stroke   : %.2f" % stroke)

    # 想让提示符在小图标里占满约 20 单位，先把它换算到 108 空间再求缩放比
    width108 = (prompt_bbox(g)[2] - prompt_bbox(g)[0]) * ADAPTIVE_VIEW / S
    k = 20.0 / width108
    print()
    print("=== 通知栏小图标（24 单位画布）===")
    for label, pts in (("chevron", chev), ("underline", under)):
        scaled = [(12 + (x - ADAPTIVE / 2) * k, 12 + (y - ADAPTIVE / 2) * k)
                  for (x, y) in pts]
        print("%-9s: " % label + " L".join("%.2f,%.2f" % p for p in scaled))
    print("stroke   : %.2f" % (stroke * k))

    print()
    for p in written:
        print("已写出", os.path.relpath(p, ROOT))


def logo_svg(size=512):
    """和位图同源的矢量版：圆角方块 + 提示符。"""
    g = prompt_geometry()
    bbox = prompt_bbox(g)
    cx, cy = (bbox[0] + bbox[2]) / 2, (bbox[1] + bbox[3]) / 2
    # 平移量要按设计画布算，最后统一乘缩放比
    dx = S / 2 - cx
    dy = S / 2 - cy + 6

    def pt(p):
        return (p[0] + dx) * size / S, (p[1] + dy) * size / S

    chev = [pt(p) for p in g["chev"]]
    under = [pt(p) for p in g["underline"]]
    stroke = g["stroke"] * size / S
    radius = size * TILE_RADIUS
    d1 = "M" + " L".join("%.2f,%.2f" % p for p in chev)
    d2 = "M" + " L".join("%.2f,%.2f" % p for p in under)
    return f"""<svg xmlns="http://www.w3.org/2000/svg" width="{size}" height="{size}" viewBox="0 0 {size} {size}">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#82E3B8"/>
      <stop offset="1" stop-color="#45BE8B"/>
    </linearGradient>
    <radialGradient id="glow" cx="0.28" cy="0.20" r="1.05">
      <stop offset="0" stop-color="#FFFFFF" stop-opacity="0.23"/>
      <stop offset="1" stop-color="#FFFFFF" stop-opacity="0"/>
    </radialGradient>
    <radialGradient id="shade" cx="0.96" cy="1.02" r="0.95">
      <stop offset="0" stop-color="#00281A" stop-opacity="0.16"/>
      <stop offset="1" stop-color="#00281A" stop-opacity="0"/>
    </radialGradient>
  </defs>
  <rect width="{size}" height="{size}" rx="{radius:.1f}" fill="url(#bg)"/>
  <rect width="{size}" height="{size}" rx="{radius:.1f}" fill="url(#glow)"/>
  <rect width="{size}" height="{size}" rx="{radius:.1f}" fill="url(#shade)"/>
  <g fill="none" stroke="#0B0F14" stroke-width="{stroke:.2f}"
     stroke-linecap="round" stroke-linejoin="round">
    <path d="{d1}"/>
    <path d="{d2}"/>
  </g>
</svg>
"""


def main():
    os.makedirs(PREVIEW, exist_ok=True)
    for name, fn in CONCEPTS.items():
        fn().convert("RGBA").resize((OUT, OUT), Image.LANCZOS).save(
            os.path.join(PREVIEW, name.split("-")[0] + ".png"))
    contact_sheet(os.path.join(PREVIEW, "对比.png"))
    homescreen(os.path.join(PREVIEW, "桌面.png"))
    sheet_for({"成品 · 随行": concept_f1},
              os.path.join(PREVIEW, "成品尺寸.png"))
    build()


if __name__ == "__main__":
    main()
