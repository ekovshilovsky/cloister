#!/bin/bash
set -euo pipefail

# Source NVM so npm/node are available (installed by base provisioning).
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"

echo "=== Installing art stack ==="

# Native CLI tooling for raster, vector, and document image processing. All
# packages are in the Ubuntu default repos so no extra apt sources are added.
sudo apt-get update -q
sudo apt-get install -y -q \
    imagemagick \
    pngquant \
    jpegoptim \
    libjpeg-turbo-progs \
    webp \
    gifsicle \
    librsvg2-bin \
    potrace \
    libimage-exiftool-perl \
    poppler-utils

# Node-based tooling that is hard to use as a one-off without a global install.
# Sharp's libvips bindings ship as prebuilt binaries; sharp-cli wraps them
# behind a CLI suitable for shell pipelines and quick conversions.
npm install -g \
    svgo \
    sharp-cli

echo "=== Art stack complete ==="
echo
echo "CLI tools available in this profile:"
echo "  convert / mogrify    - ImageMagick (resize, format conversion, composition)"
echo "  identify             - ImageMagick image metadata"
echo "  pngquant             - lossy PNG quantization"
echo "  jpegoptim, jpegtran  - JPEG optimization (lossless and lossy)"
echo "  cwebp / dwebp        - WebP encode/decode"
echo "  gifsicle             - GIF manipulation and optimization"
echo "  rsvg-convert         - SVG rasterization to PNG/PDF/PS"
echo "  potrace              - bitmap to vector (SVG/EPS) tracing"
echo "  exiftool             - read/write image metadata"
echo "  pdftoppm, pdftocairo - rasterize PDF pages to images"
echo "  svgo                 - SVG optimization (Node, global)"
echo "  sharp                - sharp-cli for high-throughput resize/convert (Node, global)"
