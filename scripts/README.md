# scripts/ — bake-off harness

Shared tooling to drive every implementation through the same flows so the [`BAKEOFF`](../docs/BAKEOFF.md)
comparison is fair.

## `gen_testdata.py`
Regenerates the shared assets in [`../testdata/`](../testdata/) (pure stdlib, no PIL):
`small.png` (64×64), `large.png` (256×256), `screenshot.png` (320×200), `sample.txt`.
Deterministic output → stable BLAKE3 hashes for round-trip assertions.

```sh
python3 scripts/gen_testdata.py
```

## `roundtrip.sh <impl>`
Spawns two isolated daemons of one implementation (separate `--config-dir` + `--socket`, a throwaway `--room`),
establishes membership, sends `sample`-text and `small.png` from A, and asserts B received them — comparing the
**image by hash** (`b3sum` if present, else `shasum -a 256`; the same tool is used on both sides).

```sh
scripts/roundtrip.sh mesh-rs
scripts/roundtrip.sh room-go
```

It only uses the shared CLI surface from [`../docs/SPEC.md`](../docs/SPEC.md). Two small hooks are impl-specific
and get wired as each app lands:
- **binary path + build command** (top `case` block),
- **`start_pair()`** — how to start the daemons and pair/join them (ticket for mesh impls, server join for `room-go`).

Run artifacts go to `scripts/.run/<impl>/` (gitignored): `a/` and `b/` device dirs, `daemon.log`s, and `out/`.
