"""Generate wallpaper.png: a dark desktop background with soft diagonal light.

Run from this directory: uv run --with pillow python3 wallpaper.py
The Dockerfile copies the PNG into the template; this script stays out of the image.
"""

from PIL import Image, ImageChops, ImageDraw, ImageFilter

W, H = 1600, 1000

# Base: dark grey, a little lighter toward the upper middle.
base = Image.new("RGB", (W, H))
px = base.load()
for y in range(H):
    for x in range(W):
        dx = (x - W * 0.45) / W
        dy = (y - H * 0.40) / H
        glow = max(0.0, 1.0 - (dx * dx + dy * dy) * 3.2)
        v = int(26 + 26 * glow)
        px[x, y] = (v, v, v + 2)

# Diagonal light bands running from the top edge down to the right, as in the
# Debian desktop this template's screen is modeled on. A bright narrow core sits
# inside a wide soft falloff.
bands = Image.new("L", (W, H), 0)
d = ImageDraw.Draw(bands)
d.polygon([(-300, -100), (500, -100), (1500, 1100), (700, 1100)], fill=30)
d.polygon([(100, -100), (420, -100), (1300, 1100), (980, 1100)], fill=40)
bands = bands.filter(ImageFilter.GaussianBlur(70))
core = Image.new("L", (W, H), 0)
d = ImageDraw.Draw(core)
d.polygon([(250, -100), (340, -100), (1150, 1100), (1060, 1100)], fill=55)
core = core.filter(ImageFilter.GaussianBlur(35))
bands = ImageChops.add(bands, core)

light = Image.new("RGB", (W, H), (205, 208, 214))
out = Image.composite(light, base, bands)
out.save("wallpaper.png", optimize=True)
print("wallpaper.png", out.size)
