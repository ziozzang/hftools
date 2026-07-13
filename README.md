# hfdown

English | [한국어](README_KO.md)

Created by Jioh L. Jung <ziozzang@gmail.com>

`hfdown` is a low-resource, resumable downloader for Hugging Face model and
dataset repositories. It resolves a revision to a Git commit, downloads files
with HTTP Range requests, and verifies them against Git blob or Git LFS hashes.

## Features

- Model and dataset repositories, pinned to a resolved commit SHA
- Full-repository or case-insensitive glob-filtered downloads
- Multipart HTTP Range downloads with resume enabled by default
- Constant-size I/O buffers; files are never held entirely in memory
- Git blob SHA-1 or Git LFS SHA-256 verification for every downloaded file
- Raw local SHA-256 and SHA-1 entries in `.sha256` and `.sha1sum`
- Hub metadata snapshots and append-only update/verification history
- Plain-text lists and per-job JSON queues
- Recursive batch verification and forced full rehashing
- Incremental updates that fetch only remotely changed files
- Static binaries for macOS, Windows, and Linux on ARM64 and x86-64

## Install and build

Download a prebuilt binary from
[GitHub Releases](https://github.com/ziozzang/hfdownload/releases), or build
with Go 1.24 or newer:

```bash
go install github.com/ziozzang/hfdownload/cmd/hfdown@latest

make build
./hfdown version
./hfdown --help
```

Use `hfdown help COMMAND` or `hfdown COMMAND --help` for command-specific
options. `hfdown --version`, `hfdown -v`, and `hfdown -V` are version aliases.

Build all six release targets:

```bash
make release
```

Outputs are written to `dist/`. Release builds use `CGO_ENABLED=0`, `netgo`,
and `osusergo`; the build script also checks that Linux outputs are statically
linked.

## Authentication

The token is read with `os.Getenv("HF_TOKEN")` by default:

```bash
export HF_TOKEN=hf_xxx
hfdown d owner/model
```

Choose another environment variable or supply a token directly:

```bash
hfdown d --token-env MY_HF_TOKEN owner/model
hfdown d --token hf_xxx owner/model
```

The environment form is safer because command-line arguments can appear in
shell history or process listings. Tokens are never stored in metadata,
checksums, configuration, or logs.

## Download a model

`download`, `dn`, and `d` are equivalent. A repository ID or full URL is
accepted. The default local directory is `<owner>_<repo>`.

```bash
hfdown download FluidInference/silero-vad-coreml
hfdown dn FluidInference/silero-vad-coreml
hfdown d https://huggingface.co/FluidInference/silero-vad-coreml
# -> ./FluidInference_silero-vad-coreml/
```

Custom destination and multipart settings:

```bash
hfdown d \
  --output ./models/silero-vad \
  --parts 8 \
  --multipart-threshold 64MiB \
  FluidInference/silero-vad-coreml
```

## Download a dataset

`dataset` and `ds` are equivalent. Dataset IDs and full `/datasets/` URLs are
accepted. The default directory naming rule is also `<owner>_<repo>`.

```bash
hfdown dataset lhoestq/demo1
hfdown ds https://huggingface.co/datasets/lhoestq/demo1
# -> ./lhoestq_demo1/
```

## Tags, revisions, and file filters

`--revision` accepts a branch, tag, or commit SHA. `--tag` is a convenient
alias that overrides `--revision`.

```bash
hfdown d --tag v1.2.0 owner/model
hfdown ds --revision 0123456789abcdef owner/dataset
```

`--filter` selects files using shell-style globs. Matching is
case-insensitive. Multiple filters are OR conditions and can be supplied by
repeating the option, using `|`, or both:

```bash
hfdown d \
  --filter '*.json|*.parquet|*_q4_?.gguf' \
  owner/model

hfdown d \
  --filter '*.json' \
  --filter '*.gguf' \
  owner/model

hfdown d \
  --tag Q4-release \
  --filter 'weights/*_q4_?.gguf|tokenizer*.json' \
  owner/model
```

Quote filter expressions so the shell does not expand `*`, `?`, or `|`.
Patterns containing `/` match the complete repository-relative path; other
patterns match the basename in any directory. A zero-match filter is reported
as an error instead of silently downloading nothing.

A filtered update checks the repository's current commit but only downloads
selected files whose remote object hashes changed. Previously managed,
unselected files are retained in the manifest and checksum files.

## Important download options

- `--revision main`: branch, tag, or commit SHA
- `--tag TAG`: tag name; overrides `--revision`
- `--filter GLOB`: include filter; repeat it or separate patterns with `|`
- `--parts 4`: parallel ranges per large file; `1` disables multipart
- `--multipart-threshold 64MiB`: minimum size for multipart mode
- `--resume=true`: resume compatible partial downloads
- `--buffer-size 1MiB`: memory buffer used by each active range
- `--retries 5`: retry count for each range
- `--token-env HF_TOKEN`: environment variable containing the token
- `--token TOKEN`: direct token value

Default download-buffer memory is approximately `parts × buffer-size`. Files
are processed sequentially and are never loaded into memory in full.

## Plain-text batch lists

Put one repository ID per line. Blank lines and lines whose first non-space
character is `;` are ignored.

```text
; Models to mirror

FluidInference/silero-vad-coreml
openai-community/gpt2
```

Download a model list:

```bash
hfdown batch --list models.txt
hfdown batch --list models.txt --output-root ./models --continue-on-error
```

Download a dataset list using the same format:

```bash
hfdown batch --type dataset --list datasets.txt --output-root ./datasets
```

Global `--tag` and `--filter` options also apply to batch list entries. See
[`models.example.txt`](models.example.txt).

## Structured JSON queue

Use a JSON queue for mixed repository types or per-job settings:

```json
{
  "output_root": "./repositories",
  "jobs": [
    {
      "repo": "owner/model",
      "revision": "v2",
      "filters": ["*.json|*_q8_0.gguf"]
    },
    {
      "repo": "owner/dataset",
      "type": "dataset",
      "filters": ["*.parquet"]
    }
  ]
}
```

```bash
hfdown batch --queue queue.json
hfdown batch --queue queue.json --continue-on-error
```

See [`queue.example.json`](queue.example.json) for all common per-job options.

## Resume behavior

Resume mode is enabled by default.

- Compatible `tmp/<file-key>/state.json` state resumes every Range at its saved
  byte offset.
- A short file at the final path is moved to `tmp/` and adopted as an existing
  prefix.
- Multipart ranges write directly to offsets in one `download.part`; no
  model-sized concatenation copy is needed.
- The final file is promoted only after its complete remote hash matches.
- After each file succeeds, the manifest, `.sha256`, and `.sha1sum` are
  checkpointed, so an interrupted batch retains checksums for every completed
  file.
- If resumed bytes fail the hash, that file is retried once from byte zero.
- Unchanged files with valid manifest records are not re-read.
- Files without a current validation record are scanned once before download.

## Progress display

Before transfer, `hfdown` prints the complete selected-file plan:

```text
plan: 42 files • 14.8 GiB total • 35 cached (9.2 GiB) • 7 remaining (5.6 GiB)
```

The progress bar shows active file, completed/total file count, percentage,
bytes, and speed. The final summary reports cached files and bytes, existing
files scanned successfully, downloaded files, network bytes, and resumed
bytes.

## Verification and updates

Metadata-cached verification avoids rehashing unchanged files:

```bash
hfdown verify --output ./FluidInference_silero-vad-coreml
```

Force a full read and rehash, or recursively verify every managed repository:

```bash
hfdown verify --output ./FluidInference_silero-vad-coreml --force
hfdown verify-batch --root ./models --force
hfdown status --output ./FluidInference_silero-vad-coreml
```

`verify-batch` continues after failures by default; add `--fail-fast` to stop
at the first failure.

Run the same download or batch command later to check for an update. The
requested revision is resolved again, and files are compared by Git blob or
Git LFS object hash. Only changed files are downloaded. A literal commit SHA
keeps the download pinned.

## Repository layout

```text
<repository-directory>/
├── <downloaded files>
├── .sha256
├── .sha1sum
├── .metadata/
│   ├── manifest.json
│   ├── repository.json
│   ├── repository-history.jsonl
│   └── verification-history.jsonl
└── tmp/                         # only while a download is incomplete
    └── <file-key>/
        ├── download.part
        └── state.json
```

`tmp/` is deliberately visible. `tmp/`, `.metadata/`, `.sha256`, and
`.sha1sum` are reserved; a remote repository containing these paths is
rejected. Legacy
`hfdown-metadata/`, `.hfdown/`, `.metadata/tmp/`, and `.hfdown/partials/`
layouts are migrated automatically.

## Hash and metadata policy

- The requested revision is resolved to a commit SHA before transfer.
- Every file URL uses that exact commit.
- Normal Git files are checked against Git blob SHA-1
  (`blob <size>\0<content>`).
- Git LFS files are checked against Hub-provided LFS SHA-256.
- Every local managed file also receives raw-content SHA-256 and SHA-1 entries.
- Raw SHA-1 in `.sha1sum` is distinct from Git blob SHA-1, which hashes a Git
  object header together with the content and remains recorded in the manifest.

After a successful download or verification, both checksum files use standard
coreutils syntax:

```bash
cd ./FluidInference_silero-vad-coreml
sha256sum -c .sha256
sha1sum -c .sha1sum
```

`.metadata/repository.json` archives the latest Hub API response, repository
type, requested revision, resolved commit, fetch time, creation time, and Hub
last-modified time. `repository-history.jsonl` records every metadata check.
`manifest.json` stores local and remote hashes plus validation state, while
`verification-history.jsonl` records verification summaries.

## Configuration file

`.hfdown.json` in the current directory is loaded automatically. Use
`--config FILE` to select another file. Command-line options override scalar
configuration values.

```json
{
  "endpoint": "https://huggingface.co",
  "revision": "main",
  "output": "",
  "filters": ["*.json|*.gguf"],
  "parts": 4,
  "multipart_threshold": 67108864,
  "buffer_size": 1048576,
  "retries": 5,
  "timeout_seconds": 30,
  "resume": true,
  "token_env": "HF_TOKEN"
}
```

Do not store a token in this file. Use `token_env` or `--token`.

## License

MIT. See [`LICENSE`](LICENSE).
