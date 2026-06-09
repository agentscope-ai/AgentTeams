# hiclaw-common

Shared runtime library for [HiClaw](https://github.com/higress-group/hiclaw) worker runtimes.

Provides:
- **`hiclaw_common.sync`** — MinIO/S3 file sync (`FileSync`, `sync_loop`, `push_loop`)
- **`hiclaw_common.matrix`** — Matrix chat relay (`MautrixRelay`)
- **`hiclaw_common.policies`** — AI tool-call policy engine (`DualAllowList`, `HistoryBuffer`)

## Install

```bash
pip install hiclaw-common            # core only
pip install "hiclaw-common[matrix]"  # + Matrix relay (mautrix)
```

## License

Apache 2.0
