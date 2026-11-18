#!/usr/bin/env python3
"""Worker side of the OCR example: run.py OUT IMAGE [IMAGE ...]

Replace `ocr_page` with your engine. This stub shells out to `tesseract`
if it is installed, so the example works end to end on a stock Mac with
Homebrew's tesseract.
"""
import shutil
import subprocess
import sys


def ocr_page(path: str) -> str:
    exe = shutil.which("tesseract") or "/opt/homebrew/bin/tesseract"
    return subprocess.run([exe, path, "-"], check=True, capture_output=True, text=True).stdout


def main():
    out, images = sys.argv[1], sys.argv[2:]
    with open(out, "w", encoding="utf-8") as f:
        for img in images:
            print("ocr", img, flush=True)
            f.write(ocr_page(img).rstrip() + "\n\f\n")


if __name__ == "__main__":
    main()
