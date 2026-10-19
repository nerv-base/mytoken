"""MyToken's icon: chat-glow's orb (ported from internal/gui/glow/orb.kage)
inside a quota ring, on a graphite squircle. Renders resources/icon.png and
a small in-app logo."""
import sys
import numpy as np
from PIL import Image, ImageFilter

STOPS = np.array([
    [0.74, 0.51, 0.95], [0.96, 0.73, 0.92], [0.55, 0.62, 1.0],
    [0.67, 0.43, 0.93], [1.0, 0.40, 0.47], [1.0, 0.73, 0.44], [0.74, 0.51, 0.95]])


def palette(x):
    x = np.mod(x, 1.0) * 6
    i = np.clip(np.floor(x).astype(int), 0, 5)
    f = (x - i)[..., None]
    return STOPS[i] * (1 - f) + STOPS[i + 1] * f


def around(uv):
    return np.arctan2(uv[..., 1], uv[..., 0]) / (2 * np.pi) + 0.5


def smoothstep(e0, e1, x):
    t = np.clip((x - e0) / (e1 - e0), 0, 1)
    return t * t * (3 - 2 * t)


def orb(size, inner, phase=4.2, energy=0.55, ss=1.0):
    """RGBA premultiplied of the orb in a size x size box."""
    ys, xs = np.mgrid[0:size, 0:size].astype(np.float64) + 0.5
    uv = np.stack([(xs - size / 2) / inner, (ys - size / 2) / inner], -1)
    r = np.linalg.norm(uv, axis=-1)
    t = phase
    out = np.zeros((size, size, 4))
    # halo
    halo = np.exp(-(r - 1) * 5) * (0.25 + 0.2 * energy) * smoothstep(1.25, 1.05, r)
    hc = palette(t * 0.05 + around(uv))
    m = r > 1
    out[m, :3] = (hc * halo[..., None])[m]
    out[m, 3] = halo[m]
    z = np.sqrt(np.maximum(1 - r * r, 0))
    w = uv * (1.1 + 0.15 * z)[..., None]
    w = w + (0.22 + 0.25 * energy) * np.stack([np.sin(w[..., 1] * 2.7 + t * 1.1), np.cos(w[..., 0] * 2.3 - t * 0.9)], -1)
    w = w + 0.12 * np.stack([np.sin(w[..., 1] * 5.1 - t * 1.7), np.cos(w[..., 0] * 4.7 + t * 1.3)], -1)
    col = np.zeros((size, size, 3)) + np.array([0.07, 0.04, 0.18])
    for i in range(5):
        f = float(i)
        c = 0.6 * np.array([np.sin(t * (0.5 + 0.13 * f) + f * 2.1), np.cos(t * (0.43 + 0.11 * f) + f * 1.3)])
        d = w - c
        k = np.exp(-(d[..., 0] ** 2 + d[..., 1] ** 2) * 3.4)
        col += palette(np.full((size, size), f / 5 + t * 0.03)) * (k * 0.85)[..., None]
    rim = (1 - z) ** 2.6
    col *= (0.7 + 0.4 * z)[..., None]
    col += palette(around(uv) + t * 0.05) * (rim * 0.8)[..., None]
    n = np.concatenate([uv, z[..., None]], -1)
    L = np.array([-0.45, -0.6, 0.66]); L /= np.linalg.norm(L)
    spec = np.maximum((n * L).sum(-1), 0) ** 60
    col += (spec * 0.7)[..., None]
    col += (0.06 * smoothstep(0.3, 1, -uv[..., 1] * z))[..., None]
    alpha = smoothstep(1, 1 - 1.5 / (inner * ss), r)
    inside = r <= 1
    out[inside, :3] = (np.minimum(col, 1) * alpha[..., None])[inside]
    out[inside, 3] = alpha[inside]
    return out


def squircle_mask(size, body, n=5.0):
    ys, xs = np.mgrid[0:size, 0:size].astype(np.float64) + 0.5
    u = np.abs(xs - size / 2) / (body / 2)
    v = np.abs(ys - size / 2) / (body / 2)
    d = (u ** n + v ** n) ** (1 / n)
    aa = 1.5 / (body / 2)
    return np.clip((1 - d) / aa + 0.5, 0, 1)


def ring(size, radius, width, frac, start=-90.0, phase=4.2):
    """Premultiplied RGBA of a ring track plus a palette arc covering frac."""
    ys, xs = np.mgrid[0:size, 0:size].astype(np.float64) + 0.5
    dx, dy = xs - size / 2, ys - size / 2
    r = np.hypot(dx, dy)
    band = np.clip(width / 2 - np.abs(r - radius) + 0.5, 0, 1)
    out = np.zeros((size, size, 4))
    track = band * 0.10
    out[..., :3] += track[..., None] * 1.0
    out[..., 3] += track
    ang = (np.degrees(np.arctan2(dy, dx)) - start) % 360 / 360  # 0 at start, clockwise
    span = frac
    on = ang <= span
    # round caps
    caps = np.zeros_like(r)
    for a in (0.0, span):
        th = np.radians(start + a * 360)
        cx, cy = np.cos(th) * radius, np.sin(th) * radius
        caps = np.maximum(caps, np.clip(width / 2 - np.hypot(dx - cx, dy - cy) + 0.5, 0, 1))
    arc = np.maximum(band * on, caps)
    # caps take their end's color (the start cap's pixels wrap to ~1)
    angc = np.where(ang > span, np.where(ang > (1 + span) / 2, 0.0, span), ang)
    col = palette(0.55 + angc * 0.9 + phase * 0.03)
    out[..., :3] = out[..., :3] * (1 - arc[..., None]) + col * arc[..., None]
    out[..., 3] = out[..., 3] * (1 - arc) + arc
    return out


def over(dst, src):
    a = src[..., 3:4]
    dst[..., :3] = src[..., :3] + dst[..., :3] * (1 - a)
    dst[..., 3:4] = a + dst[..., 3:4] * (1 - a)
    return dst


def icon(size=1024, ss=2, app=True):
    S = size * ss
    body = S * (824 / 1024) if app else S
    canvas = np.zeros((S, S, 4))
    ys, xs = np.mgrid[0:S, 0:S].astype(np.float64) / S
    # graphite, a touch of violet toward the bottom right
    top = np.array([0.155, 0.150, 0.190]); bot = np.array([0.060, 0.055, 0.085])
    g = np.clip(0.65 * ys + 0.35 * xs, 0, 1)[..., None]
    bg = top * (1 - g) + bot * g
    mask = squircle_mask(S, body)
    # soft inner light at the top edge
    bg += (0.05 * np.clip(1 - ys / 0.35, 0, 1))[..., None]
    bgl = np.concatenate([bg * mask[..., None], mask[..., None]], -1)
    if app:
        # drop shadow under the squircle
        sh = Image.fromarray((squircle_mask(S, body) * 255).astype(np.uint8))
        sh = sh.filter(ImageFilter.GaussianBlur(S * 0.018))
        sh = np.roll(np.asarray(sh, np.float64) / 255, int(S * 0.012), axis=0) * 0.45
        canvas = over(canvas, np.dstack([np.zeros((S, S, 3)), sh]))
    canvas = over(canvas, bgl)
    # quota ring
    R = body * 0.335
    canvas = over(canvas, ring(S, R, body * 0.052, 0.72) * mask[..., None])
    # orb
    inner = body * 0.215
    o = orb(S, inner, ss=ss)
    canvas = over(canvas, o * mask[..., None])
    img = Image.fromarray((np.clip(canvas, 0, 1) * 255).astype(np.uint8), "RGBA")
    # un-premultiply for PNG
    arr = np.asarray(img).astype(np.float64)
    a = arr[..., 3:4] / 255
    arr[..., :3] = np.where(a > 0, arr[..., :3] / np.maximum(a, 1e-6), 0)
    img = Image.fromarray(np.clip(arr, 0, 255).astype(np.uint8), "RGBA")
    return img.resize((size, size), Image.LANCZOS)


if __name__ == "__main__":
    out = sys.argv[1]
    icon(1024, 2, True).save(out + "/icon.png")
    icon(128, 3, False).save(out + "/logo.png")
