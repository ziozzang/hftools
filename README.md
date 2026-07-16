# hfdown

English | [한국어](README_KO.md)

Created by Jioh L. Jung <ziozzang@gmail.com> — [GitHub](https://github.com/ziozzang/hfdownload)

`hfdown` is a low-resource, resumable downloader for Hugging Face model and
dataset repositories. It resolves a revision to a Git commit, downloads files
with HTTP Range requests, and verifies them against Git blob or Git LFS hashes.

## Features

- Model and dataset repositories, pinned to a resolved commit SHA
- Full-repository or case-insensitive glob-filtered downloads
- Multipart HTTP Range downloads with resume enabled by default
- Randomized-backoff retries for server errors (5xx/429) and stalled or too-slow
  connections, with an optional retry-until-success mode for extended outages
- Constant-size I/O buffers; files are never held entirely in memory
- Git blob SHA-1 or Git LFS SHA-256 verification for every downloaded file
- Raw local SHA-256 and SHA-1 entries in `.sha256` and `.sha1sum`
- Hub metadata snapshots and append-only update/verification history
- Plain-text lists and per-job JSON queues
- Recursive batch verification and forced full rehashing
- Incremental updates that fetch only remotely changed files
- Convert to and from the Hugging Face cache layout for offline / air-gapped use
- Serve local downloads over the Hugging Face URL scheme to an offline fleet
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
- `--retries 5`: retries per range and per API call for transient errors (5xx, 429, network); `-1` retries until success. Genuine client errors (404, 401, 403) are never retried
- `--retry-min-wait 1` / `--retry-max-wait 300`: bounds (seconds) for the randomized backoff between retries; the wait grows from the minimum up to the maximum with jitter, so during an outage it settles between roughly half and all of the maximum
- `--stall-timeout 60`: reconnect and resume a range after this many seconds without progress; `0` disables stall detection
- `--min-speed 1MiB`: reconnect and resume a range (connection) that averages below this rate over a short window; `0` disables the floor. With `--parts N` the floor is per connection, so the whole-file rate can be up to `N ×` this value
- `--min-speed-window 5`: averaging window (seconds) used by `--min-speed`; a shorter window reacts faster but is more sensitive to brief dips
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
- A connection that goes silent for `--stall-timeout` seconds, or that averages
  below `--min-speed` over `--min-speed-window` seconds, is dropped and resumed
  on a fresh connection from the last received offset.
- Transient failures — server errors (5xx), rate limiting (429), network drops,
  and the stall/slow aborts above — are retried with randomized backoff, both
  for range downloads and for the metadata API call. Set `--retries -1` to keep
  retrying until success, which rides out extended outages (for example repeated
  503s) instead of failing the run. Genuine client errors (404, 401, 403) are
  terminal and stop immediately. Each retry prints a line such as `... HTTP 503;
  retrying in 4m12s (resume at 128.0 MiB)`.
- The `--retries` budget counts *consecutive* failures: any forward progress
  resets it and reconnects immediately, so once a server recovers a large file
  keeps going as long as it advances rather than exhausting a fixed count.

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

## Offline use and the Hugging Face cache

For air-gapped machines you can move a download between hfdown's flat layout and
the `huggingface_hub` cache layout, so the Python libraries (`transformers`,
`diffusers`, `datasets`, …) can load it offline.

```bash
# Flat download -> HF cache (blobs + snapshot symlinks + refs)
hfdown cache-export --output ./owner_model --cache ~/.cache/huggingface/hub

# HF cache snapshot -> flat download directory (hashes and verifies every file)
hfdown cache-import --repo owner/model --cache ~/.cache/huggingface/hub --output ./owner_model
```

- `cache-export` reads the download's manifest and writes
  `models--owner--model/{blobs,snapshots/<commit>,refs}`. Blobs are named by
  their Hugging Face etag — the LFS SHA-256 for LFS files, the git blob SHA-1
  for regular files — which hfdown already records, so nothing is re-hashed.
  Blobs are hardlinked from the source by default (`--copy` to copy instead);
  snapshot entries are relative symlinks, falling back to copies where symlinks
  are unavailable (e.g. Windows).
- `cache-import` resolves the commit from `refs/<revision>`, then hashes each
  snapshot file and checks it against its content-addressed blob name, so a
  corrupt transfer is caught. It writes a fresh hfdown manifest, `.sha256`, and
  `.sha1sum`, making the result a first-class hfdown directory you can `verify`.
- `--cache` defaults to `$HF_HUB_CACHE`, then `$HF_HOME/hub`, then
  `~/.cache/huggingface/hub`. `--type dataset` handles dataset repositories.

To use an exported cache offline, point the HF libraries at it and disable
network access:

```bash
export HF_HOME=/path/to/huggingface        # parent of the hub/ directory
export HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
```

Related cache utilities:

- `hfdown cache-list --cache DIR` lists the repositories stored in a cache.
- `hfdown cache-verify --cache DIR [--repo OWNER/NAME]` rehashes every blob and
  checks it against its content-addressed name. It needs no manifest, so it
  validates a cache received across an air gap on its own.
- `hfdown cache-import-batch --cache DIR --output-root OUT` imports every
  repository in a cache into flat directories at once. Import is resumable — an
  already-present, correct file is reused rather than re-copied.
- `hfdown cache-export --archive repo.tar` additionally writes a `.tar` bundle
  (and `repo.tar.sha256`) of the exported repository for transfer on physical
  media; unpack it under a cache root and it is ready to use.

## Serve downloads to an offline fleet

On an isolated network, run one host as a mirror and let other machines download
from it with hfdown (or any client using the Hugging Face URL scheme):

```bash
# On the mirror host (holds hfdown download directories under ./repos):
hfdown serve --root ./repos --addr 0.0.0.0:8080

# On another machine on the same network:
hfdown download --endpoint http://mirror-host:8080 owner/model
```

`serve` indexes every hfdown repository under `--root` (each a directory with a
`.metadata/manifest.json`) and answers the Hub metadata API and the ranged
`resolve` download endpoint from local files, so range requests, resume, and the
retry/stall logic all work against it unchanged. It serves the revision each
repository was downloaded at (its branch or tag name, or the commit). Pass
`--token-env VAR` to require `Authorization: Bearer <value-of-VAR>`.

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
  "retry_min_wait_seconds": 1,
  "retry_max_wait_seconds": 300,
  "timeout_seconds": 30,
  "stall_timeout_seconds": 60,
  "min_speed": 0,
  "min_speed_window_seconds": 5,
  "resume": true,
  "token_env": "HF_TOKEN"
}
```

Do not store a token in this file. Use `token_env` or `--token`.

## License

MIT. See [`LICENSE`](LICENSE).
