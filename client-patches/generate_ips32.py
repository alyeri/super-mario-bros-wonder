#!/usr/bin/env python3
"""Generate the exact-build IPS32 patch used by Wonder 1.2.1."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path


BUILD_ID = "FF773E90972D544EB79406EAA65396D53C43EFB9"
HEADER_SIZE = 0x100
RECORDS = (
    (0xB03628, bytes.fromhex("AA E2 40 39"), bytes.fromhex("2A 00 80 52")),
    (0xB02CBC, bytes.fromhex("00 04 00 35"), bytes.fromhex("1F 20 03 D5")),
    (0xB02BA4, bytes.fromhex("C1 0D 00 54"), bytes.fromhex("1F 20 03 D5")),
)


def build_patch() -> bytes:
    data = bytearray(b"IPS32")
    for offset, _expected, replacement in RECORDS:
        data.extend(offset.to_bytes(4, "big"))
        data.extend(len(replacement).to_bytes(2, "big"))
        data.extend(replacement)
    data.extend(b"EEOF")
    return bytes(data)


def verify_flat(path: Path) -> None:
    image = path.read_bytes()
    for nso_offset, expected, _replacement in RECORDS:
        flat_offset = nso_offset - HEADER_SIZE
        actual = image[flat_offset : flat_offset + len(expected)]
        if actual != expected:
            raise SystemExit(
                f"unexpected bytes at flat 0x{flat_offset:X}: "
                f"expected {expected.hex(' ')}, got {actual.hex(' ')}"
            )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--output",
        type=Path,
        default=Path(f"{BUILD_ID}.ips"),
        help="output IPS32 path",
    )
    parser.add_argument(
        "--verify-flat",
        type=Path,
        help="optional decompressed main image whose original instructions are checked",
    )
    args = parser.parse_args()

    if args.verify_flat:
        verify_flat(args.verify_flat)
    patch = build_patch()
    args.output.write_bytes(patch)
    print(f"wrote {args.output} ({len(patch)} bytes, sha256={hashlib.sha256(patch).hexdigest()})")


if __name__ == "__main__":
    main()
