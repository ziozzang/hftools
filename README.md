# hfdown

`hfdown` is a low-resource, resumable downloader for complete Hugging Face model
repositories. It resolves a revision to a Git commit, downloads every file with
HTTP Range requests, and verifies the result against Git blob or Git LFS hashes.

## Features

- Full model-repository downloads pinned to a resolved commit SHA
- Multipart HTTP Range downloads with resume enabled by default
- Adoption of an existing short file as a resumable prefix
- Constant-size I/O buffers and no model-sized in-memory buffering
- Per-file Git blob SHA-1 or Git LFS SHA-256 verification
- A local SHA-256 checksum file for every downloaded model file
- Repository metadata snapshots and append-only verification history
- Plain-text model lists and structured JSON queues
- Recursive verification of every downloaded model under a directory
- Incremental updates: only files whose remote object hash changed are fetched
- Static cross-platform release binaries

## Build

Go 1.24 or newer is required.

Download a prebuilt static binary from
[GitHub Releases](https://github.com/ziozzang/hfdownload/releases), or install
from source:

```bash
go install github.com/ziozzang/hfdownload/cmd/hfdown@latest
```

```bash
make build
./hfdown version
```

Build all release targets:

```bash
make release
```

Release binaries are written to `dist/` for:

- macOS ARM64 and x86_64
- Windows ARM64 and x86_64
- Linux ARM64 and x86_64

All release builds use `CGO_ENABLED=0`, `netgo`, and `osusergo`. Linux outputs
are additionally checked with `file` to ensure they are statically linked.

## Authentication

By default, `hfdown` reads the token with `os.Getenv("HF_TOKEN")`.

```bash
export HF_TOKEN=hf_xxx
hfdown download owner/model
```

To use a differently named environment variable:

```bash
hfdown download --token-env MY_HF_TOKEN owner/model
```

A token can also be passed directly:

```bash
hfdown download --token hf_xxx owner/model
```

The environment-variable form is recommended because command-line arguments
may be visible in shell history or process listings. Tokens are never written
to `.metadata`, `.sha256`, configuration files, or logs.

## Download one model

Both repository IDs and full Hugging Face URLs are accepted. If `--output` is
omitted, the local directory name is `<owner>_<repo>`.

```bash
hfdown download FluidInference/silero-vad-coreml
# -> ./FluidInference_silero-vad-coreml/

hfdown download https://huggingface.co/FluidInference/silero-vad-coreml
# -> ./FluidInference_silero-vad-coreml/
```

Custom destination and multipart settings:

```bash
hfdown download \
  --output ./models/silero-vad \
  --parts 8 \
  --multipart-threshold 64MiB \
  FluidInference/silero-vad-coreml
```

Important options:

- `--revision main`: branch, tag, or commit SHA
- `--parts 4`: parallel ranges per large file; `1` disables multipart
- `--multipart-threshold 64MiB`: minimum file size for multipart mode
- `--resume=true`: resume compatible partial downloads
- `--buffer-size 1MiB`: memory buffer used by each active range
- `--retries 5`: retry count for each range
- `--token-env HF_TOKEN`: environment variable holding the token
- `--token TOKEN`: direct token value

Default download-buffer memory is approximately `parts × buffer-size`. Files
are processed one at a time and are never loaded into memory in full.

## Download a plain-text model list

The simplest batch format contains one model per line. Blank lines and lines
whose first non-space character is `;` are ignored.

```text
; Voice models

FluidInference/silero-vad-coreml
hf-internal-testing/tiny-random-bert

; Full URLs are valid too
https://huggingface.co/openai-community/gpt2
```

Run the list:

```bash
hfdown batch --list models.txt
hfdown batch --list models.txt --output-root ./models --continue-on-error
```

Without `--output-root`, models are created under the current directory. The
example above creates directories such as
`FluidInference_silero-vad-coreml/`.

See [`models.example.txt`](models.example.txt) for a complete example.

## Structured JSON queue

Use a JSON queue when models need different revisions, output paths, part
counts, or buffer settings.

```bash
hfdown batch --queue queue.json
hfdown batch --queue queue.json --continue-on-error
```

See [`queue.example.json`](queue.example.json).

## Resume behavior

Resume mode is enabled by default.

- A compatible `tmp/<file-key>/state.json` resumes each Range at its saved
  byte offset.
- A file at the final output path that is shorter than the remote file is moved
  to `tmp/` and adopted as an already downloaded prefix.
- The final file is promoted only after its complete hash matches the remote
  object hash.
- If resumed bytes fail the final hash, they are removed and that file is
  downloaded once from byte zero during the same run.
- Files already validated in the manifest are not read again when their remote
  object hash, size, and modification time are unchanged.
- Files without a matching validation record are scanned once before deciding
  whether a download is necessary.

Multipart ranges write directly to their offsets in one `download.part` file.
This avoids a separate part-concatenation pass and model-sized duplicate disk
usage.

## Progress display

Before downloading, `hfdown` prints the complete plan:

```text
plan: 42 files • 14.8 GiB total • 35 cached (9.2 GiB) • 7 remaining (5.6 GiB)
```

The progress bar tracks completed bytes against total repository size and
shows the active file, file count, percentage, bytes, and transfer speed. The
completion summary reports:

- Total and cached file counts
- Cached bytes
- Existing files verified by scanning
- Files actually downloaded
- Bytes received from the network
- Partial bytes reused by resume

## Verification

Verify one model. The default mode reuses unchanged validation records:

```bash
hfdown verify --output ./FluidInference_silero-vad-coreml
```

Force every file to be read and rehashed:

```bash
hfdown verify --output ./FluidInference_silero-vad-coreml --force
```

Recursively verify all downloaded models under a directory:

```bash
hfdown verify-batch --root ./models
hfdown verify-batch --root ./models --force
```

Batch verification continues after model failures by default. Use
`--fail-fast` to stop at the first failed model.

Inspect recorded status:

```bash
hfdown status --output ./FluidInference_silero-vad-coreml
```

## Incremental model updates

Run the same download command or queue again to check for model updates:

```bash
hfdown batch --list models.txt --output-root ./models --continue-on-error
```

The requested revision is resolved again. Files are compared by Git blob or
Git LFS object hash, so an updated commit only downloads changed files. Files
with unchanged object hashes are reused across commits. A revision set to a
specific commit SHA remains pinned to that commit.

## Files created in each model directory

```text
<model-directory>/
├── <model files>
├── .sha256
├── .metadata/
│   ├── manifest.json
│   ├── repository.json
│   ├── repository-history.jsonl
│   └── verification-history.jsonl
└── tmp/                         # exists only while a download is incomplete
    └── <file-key>/
        ├── download.part
        └── state.json
```

The `tmp/` and `.metadata/` paths are reserved for `hfdown`. A remote model
containing either path is rejected to prevent accidental collisions.

Legacy `hfdown-metadata/`, `.hfdown/`, `.metadata/tmp/`, and
`.hfdown/partials/` layouts are migrated automatically.

## Hash and metadata policy

- The repository revision is resolved to a commit SHA before downloading.
- Every download URL uses that resolved commit.
- Normal Git files are checked against Git blob SHA-1
  (`blob <size>\0<content>`).
- Git LFS files are checked against the Hub-provided LFS SHA-256.
- Every local model file receives an additional raw-content SHA-256 entry.

After a successful download or verification, `.sha256` is written in standard
`sha256sum` format:

```bash
cd ./FluidInference_silero-vad-coreml
sha256sum -c .sha256
```

`.metadata/repository.json` stores the latest Hub metadata response together
with the requested revision, resolved commit, fetch time, creation time, and
last-modified time. `.metadata/repository-history.jsonl` keeps a compact
append-only history of metadata checks.

`.metadata/manifest.json` stores file hashes and verification state.
`.metadata/verification-history.jsonl` stores verification-run summaries.

## Configuration file

If `.hfdown.json` exists in the current directory, it is loaded automatically.
Use `--config FILE` to select a different file. Command-line options override
configuration values.

```json
{
  "endpoint": "https://huggingface.co",
  "revision": "main",
  "output": "",
  "parts": 4,
  "multipart_threshold": 67108864,
  "buffer_size": 1048576,
  "retries": 5,
  "timeout_seconds": 30,
  "resume": true,
  "token_env": "HF_TOKEN"
}
```

Do not store a token in the configuration file. Use `token_env` or `--token`.

## License

MIT. See [`LICENSE`](LICENSE).
