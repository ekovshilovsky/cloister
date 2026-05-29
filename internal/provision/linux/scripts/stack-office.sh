#!/bin/bash
set -euo pipefail

# Source NVM so npm/node are available (installed by base provisioning).
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"

echo "=== Installing office stack ==="

# Headless office-document tooling. LibreOffice is installed component-by-component
# rather than via the libreoffice meta-package to avoid pulling the database
# (Base) and formula editor (Math) modules, which are rarely needed for
# document conversion workloads and inflate the install footprint substantially.
#
# The primary motivation is to enable round-trip rendering of presentation,
# spreadsheet, and word-processing formats inside the VM — most notably to
# support 'marp --pptx', which shells out to 'soffice --headless --convert-to
# pptx' under the hood. Without LibreOffice present, Marp silently degrades
# to HTML output and PPTX generation has to be done off-VM.
sudo apt-get update -q
sudo apt-get install -y -q \
    libreoffice-core \
    libreoffice-impress \
    libreoffice-writer \
    libreoffice-calc \
    libreoffice-draw \
    pandoc \
    fonts-liberation \
    fonts-dejavu

echo "=== Office stack complete ==="
echo
echo "CLI tools available in this profile:"
echo "  soffice              - LibreOffice headless engine (--convert-to pptx/docx/xlsx/pdf)"
echo "  libreoffice          - Same engine, alternative entry point"
echo "  pandoc               - Universal document converter (markdown/docx/html/latex/...)"
echo
echo "Common one-liners:"
echo "  soffice --headless --convert-to pptx deck.md             # via Marp + LibreOffice"
echo "  soffice --headless --convert-to pdf report.docx"
echo "  pandoc -f markdown -t docx notes.md -o notes.docx"
