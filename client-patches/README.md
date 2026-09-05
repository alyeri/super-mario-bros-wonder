# Wonder 1.2.1 client patch

Target:

- title ID: `010015100B514000`
- build ID: `FF773E90972D544EB79406EAA65396D53C43EFB9`

The patch changes three ARM64 instructions and is gated to this exact build:

| Flat offset | NSO/IPS32 offset | Original | Replacement | Purpose |
| --- | --- | --- | --- | --- |
| `0xB03528` | `0xB03628` | `AA E2 40 39` | `2A 00 80 52` | accept the configured local certificate |
| `0xB02BBC` | `0xB02CBC` | `00 04 00 35` | `1F 20 03 D5` | skip peer-name rejection branch |
| `0xB02AA4` | `0xB02BA4` | `C1 0D 00 54` | `1F 20 03 D5` | skip peer-name rejection branch |

Use `nextendo-ryujinx-wonder-1.2.1.patch` to integrate the replacements into the Nextendo Ryujinx patch table. It was generated from clean commit `3006d8b3b3cf2573a13887a9808c761530d4dc57` and does not include diagnostic observers or changes for other games.

Use `generate_ips32.py` only when a standalone IPS32 file is appropriate for the client deployment. Its optional verification reads a decompressed `main` image locally; it never modifies that image.
