# hftools

English | [한국어](README_KO.md)

Created by Jioh L. Jung <ziozzang@gmail.com> — [GitHub](https://github.com/ziozzang/hftools)

`hftools` is a low-resource, resumable toolkit for Hugging Face model and
dataset repositories. It resolves a revision to a Git commit, downloads files
with HTTP Range requests, and verifies them against Git blob or Git LFS hashes —
plus a suite of inspection, security, provenance, and maintenance commands built
around the same integrity-first, offline-friendly core.

> Formerly `hfdown`. The binary and command are now `hftools`; on-disk metadata
> and the legacy `.hfdown.json` config are still read for backward compatibility.

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
- Incremental updates that fetch only remotely changed files, report how the
  revision moved, and optionally `--prune` files deleted upstream
- Models, datasets, and **Spaces** (`--type space` / the `space` command)
- Inspect remote repositories without downloading: `info`, `ls`, `diff`, and
  `peek` (read a safetensors/GGUF header via a single Range request)
- Discover on the Hub: `search` models/datasets/spaces, `refs` (branches/tags),
  and `whoami`
- Publish to the Hub with a write token: `upload` files/folders (Git LFS aware,
  one commit), `repo` create/delete, and `tag` create/list/delete
- Reuse an existing `huggingface-cli login` token automatically
- `get` a single file (to a path or stdout); `dry-run` any download
- Scan pickle/torch checkpoints for unsafe imports (`scan`)
- ed25519 provenance signatures over the content manifest (`sign` / `verify-sig`),
  with the signer identity and signing time bound into the signature and shown on
  every verify, plus a `~/.hftools` signing identity and trusted-key registry (`key`)
- Storage tools: `du`, `gc`, cross-repo hardlink `dedup`, and HF-cache `cache-gc`
- `repair` corrupt downloads, `watch` for upstream changes, `doctor` the environment
- Convert to and from the Hugging Face cache layout for offline / air-gapped use
- Serve local downloads over the Hugging Face URL scheme to an offline fleet
- Shell completion for bash, zsh, and fish (`completion`)
- Self-update to the latest release with SHA-256 verification (`update`)
- Static binaries for macOS, Windows, and Linux on ARM64 and x86-64

## Install and build

Download a prebuilt binary from
[GitHub Releases](https://github.com/ziozzang/hftools/releases), or build
with Go 1.24 or newer:

```bash
go install github.com/ziozzang/hftools/cmd/hftools@latest

make build
./hftools version
./hftools --help
```

Use `hftools help COMMAND` or `hftools COMMAND --help` for command-specific
options. `hftools --version`, `hftools -v`, and `hftools -V` are version aliases.

Build all six release targets:

```bash
make release
```

Outputs are written to `dist/`. Release builds use `CGO_ENABLED=0`, `netgo`,
and `osusergo`; the build script also checks that Linux outputs are statically
linked.

## Update the binary

```bash
hftools update --check          # report whether a newer release exists
hftools update                  # download, verify, and replace this binary
hftools update --version v0.8.1 # install a specific release tag
```

`update` fetches the matching build for your platform from the GitHub release,
verifies its SHA-256 against the release `SHA256SUMS` before installing, replaces
the running executable in place (moving the old one aside on Windows), and prints
the new release's notes. If the binary lives in a system directory, run it with
elevated privileges. Set `GITHUB_TOKEN` to raise the API rate limit.

hftools also prints a one-line notice after a command when a newer release is
available:

```text
hftools 0.9.2 is available (you have 0.9.1). Run 'hftools update' to upgrade.
```

The version lookup runs in the **background** and the foreground never waits on
it, so an offline or air-gapped machine is never slowed down — the check simply
times out and soft-fails silently. The notice itself is printed from a cached
result (refreshed at most once a day), only when output is a terminal, so scripts
and piped output are never disturbed. It never installs anything; upgrading is
always the explicit `hftools update`. `hftools doctor` shows the current check
status (up to date / update available / could not reach GitHub). Disable the
whole thing with `HFTOOLS_NO_UPDATE_CHECK=1`.

## Authentication

The token is read with `os.Getenv("HF_TOKEN")` by default:

```bash
export HF_TOKEN=hf_xxx
hftools d owner/model
```

Choose another environment variable or supply a token directly:

```bash
hftools d --token-env MY_HF_TOKEN owner/model
hftools d --token hf_xxx owner/model
```

The environment form is safer because command-line arguments can appear in
shell history or process listings. Tokens are never stored in metadata,
checksums, configuration, or logs.

If neither `--token` nor the token env var is set, hftools falls back to the
`huggingface_hub` token file (`$HF_TOKEN_PATH`, or `$HF_HOME/token`, default
`~/.cache/huggingface/token`), so a token created by `huggingface-cli login`
works without any extra configuration. `hftools whoami` shows who the resolved
token authenticates as.

## Download a model

`download`, `dn`, and `d` are equivalent. A repository ID or full URL is
accepted. The default local directory is `<owner>_<repo>`.

```bash
hftools download FluidInference/silero-vad-coreml
hftools dn FluidInference/silero-vad-coreml
hftools d https://huggingface.co/FluidInference/silero-vad-coreml
# -> ./FluidInference_silero-vad-coreml/
```

Custom destination and multipart settings:

```bash
hftools d \
  --output ./models/silero-vad \
  --parts 8 \
  --multipart-threshold 64MiB \
  FluidInference/silero-vad-coreml
```

## Download a dataset

`dataset` and `ds` are equivalent. Dataset IDs and full `/datasets/` URLs are
accepted. The default directory naming rule is also `<owner>_<repo>`.

```bash
hftools dataset lhoestq/demo1
hftools ds https://huggingface.co/datasets/lhoestq/demo1
# -> ./lhoestq_demo1/
```

## Tags, revisions, and file filters

`--revision` accepts a branch, tag, or commit SHA. `--tag` is a convenient
alias that overrides `--revision`.

```bash
hftools d --tag v1.2.0 owner/model
hftools ds --revision 0123456789abcdef owner/dataset
```

`--filter` selects files using shell-style globs. Matching is
case-insensitive. Multiple filters are OR conditions and can be supplied by
repeating the option, using `|`, or both:

```bash
hftools d \
  --filter '*.json|*.parquet|*_q4_?.gguf' \
  owner/model

hftools d \
  --filter '*.json' \
  --filter '*.gguf' \
  owner/model

hftools d \
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
hftools batch --list models.txt
hftools batch --list models.txt --output-root ./models --continue-on-error
```

Download a dataset list using the same format:

```bash
hftools batch --type dataset --list datasets.txt --output-root ./datasets
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
hftools batch --queue queue.json
hftools batch --queue queue.json --continue-on-error
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

Before transfer, `hftools` prints the complete selected-file plan:

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
hftools verify --output ./FluidInference_silero-vad-coreml
```

Force a full read and rehash, or recursively verify every managed repository:

```bash
hftools verify --output ./FluidInference_silero-vad-coreml --force
hftools verify-batch --root ./models --force
hftools status --output ./FluidInference_silero-vad-coreml
```

`verify-batch` continues after failures by default; add `--fail-fast` to stop
at the first failure.

`verify` and `verify-batch` can fold the security scan and signature check into
the same pass, so one command proves a repository is intact, safe to load, and
from a trusted signer:

```bash
hftools verify-batch --root ./models --scan --verify-sig
```

`--scan` flags pickle/torch files with unsafe imports (a critical finding fails
the repository); `--verify-sig` verifies each repository's stored signature
(add `--pubkey <name|hex|PEM|file>` to pin, or rely on your trusted-key
registry). Long scans and verifies are interruptible with **Ctrl+C** (or **ESC**
/ **q** on a terminal).

A signature check reports **who** signed the repository and **when**, so a
verification leaves an auditable trace of who is answerable for the content:

```
  sig: OK 0f4095319cd38e03 (trusted: alice)
       signed by alice@corp.example at 2026-07-17T09:12:44Z
```

The signer label and timestamp are part of the signed bytes, so editing either
one in `signature.json` invalidates the signature rather than silently
misattributing a download. Set your label once with
`hftools key init --signer you@example.com`.

**If you rely on the signer label for attribution, enforce it.** Signatures
written before schema v2 do not cover the label, and an attacker can rewrite a v2
record as a v1 one with a forged signer — it still verifies, and is reported as
`UNVERIFIED signer "..." — not covered by the signature`. To reject such records
outright rather than trusting the reader to notice the warning:

```bash
hftools verify --output ./repo --verify-sig --require-signed-identity
hftools verify-batch --root ./models --verify-sig --require-signed-identity
```

Set `require_signed_identity: true` in `~/.hftools/config.yaml` to enforce it
everywhere without passing the flag. Pinning the key (`--pubkey`) or trusting it
in the registry remains the strongest control: it proves which key signed,
independent of any label.

Run the same download or batch command later to check for an update. The
requested revision is resolved again, and files are compared by Git blob or
Git LFS object hash — only changed files are downloaded, and the run reports
how the repository moved:

```
update: a7121eefebf9 → 71034c5d8bde • 1 new • 1 changed • 8 unchanged • 1 removed upstream
  removed upstream: generation_config.json
note: 1 file(s) removed upstream are still on disk; re-run with --prune to delete them
```

Files that disappeared upstream are dropped from the manifest but left on disk
unless you pass `--prune`, which deletes them so the directory mirrors the
revision exactly. Only files this download produced are ever deleted — anything
you added alongside them is untouched — and `--prune` is ignored when `--filter`
is in effect, since an unselected file cannot be told apart from a deleted one.
A literal commit SHA keeps the download pinned.

## Offline use and the Hugging Face cache

For air-gapped machines you can move a download between hftools's flat layout and
the `huggingface_hub` cache layout, so the Python libraries (`transformers`,
`diffusers`, `datasets`, …) can load it offline.

```bash
# Flat download -> HF cache (blobs + snapshot symlinks + refs)
hftools cache-export --output ./owner_model --cache ~/.cache/huggingface/hub

# HF cache snapshot -> flat download directory (hashes and verifies every file)
hftools cache-import --repo owner/model --cache ~/.cache/huggingface/hub --output ./owner_model
```

- `cache-export` reads the download's manifest and writes
  `models--owner--model/{blobs,snapshots/<commit>,refs}`. Blobs are named by
  their Hugging Face etag — the LFS SHA-256 for LFS files, the git blob SHA-1
  for regular files — which hftools already records, so nothing is re-hashed.
  Blobs are hardlinked from the source by default (`--copy` to copy instead);
  snapshot entries are relative symlinks, falling back to copies where symlinks
  are unavailable (e.g. Windows).
- `cache-import` resolves the commit from `refs/<revision>`, then hashes each
  snapshot file and checks it against its content-addressed blob name, so a
  corrupt transfer is caught. It writes a fresh hftools manifest, `.sha256`, and
  `.sha1sum`, making the result a first-class hftools directory you can `verify`.
- `--cache` defaults to `$HF_HUB_CACHE`, then `$HF_HOME/hub`, then
  `~/.cache/huggingface/hub`. `--type dataset` handles dataset repositories.

To use an exported cache offline, point the HF libraries at it and disable
network access:

```bash
export HF_HOME=/path/to/huggingface        # parent of the hub/ directory
export HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
```

Related cache utilities:

- `hftools cache-list --cache DIR` lists the repositories stored in a cache.
- `hftools cache-verify --cache DIR [--repo OWNER/NAME]` rehashes every blob and
  checks it against its content-addressed name. It needs no manifest, so it
  validates a cache received across an air gap on its own.
- `hftools cache-import-batch --cache DIR --output-root OUT` imports every
  repository in a cache into flat directories at once. Import is resumable — an
  already-present, correct file is reused rather than re-copied.
- `hftools cache-export --archive repo.tar` additionally writes a `.tar` bundle
  (and `repo.tar.sha256`) of the exported repository for transfer on physical
  media; unpack it under a cache root and it is ready to use.

## Serve downloads to an offline fleet

On an isolated network, run one host as a mirror and let other machines download
from it with hftools (or any client using the Hugging Face URL scheme):

```bash
# On the mirror host (holds hftools download directories under ./repos):
hftools serve --root ./repos --addr 0.0.0.0:8080

# On another machine on the same network:
hftools download --endpoint http://mirror-host:8080 owner/model
```

`serve` indexes every hftools repository under `--root` (each a directory with a
`.metadata/manifest.json`) and answers the Hub metadata API and the ranged
`resolve` download endpoint from local files, so range requests, resume, and the
retry/stall logic all work against it unchanged. It serves the revision each
repository was downloaded at (its branch or tag name, or the commit). Pass
`--token-env VAR` to require `Authorization: Bearer <value-of-VAR>`. A browsable
index is served at `/` and a liveness probe at `/health`.

## Inspect and discover on the Hub

```bash
hftools info owner/model              # summary: files, size, LFS, gated, tags
hftools ls --long owner/model         # per-file sizes and LFS markers
hftools ls --filter '*.safetensors' owner/model
hftools refs owner/model              # branches and tags
hftools peek owner/model model.safetensors   # tensor count, dtypes, params
hftools diff --output ./owner_model   # compare a local download to the remote
hftools download --dry-run owner/model

hftools search --limit 10 --filter text-generation llama   # search models
hftools search --type dataset squad                        # search datasets
hftools whoami                        # who your token authenticates as
```

`peek` reads only the file header via a single HTTP Range request, so it reports
the tensor count, dtypes, shapes, and parameter total of a multi-gigabyte
safetensors or GGUF checkpoint by transferring a few megabytes. `info`, `ls`,
`refs`, `diff`, `du`, `peek`, `search`, and `scan` all accept `--json` for
scripting.

Spaces are supported everywhere a repository type applies — pass `--type space`
to the inspection commands, or use the `space` command to download one:

```bash
hftools space gradio/hello_world
hftools info --type space gradio/hello_world
```

## Fetch a single file

```bash
hftools get owner/model config.json                 # -> ./config.json (verified)
hftools get owner/model config.json -o -            # to stdout
```

`get` (alias `cat`) resolves the commit, downloads one file, and verifies it
against the Hub hash before writing it.

## Publish to the Hub

Writing to the Hub needs a token with **write** access (create one at
<https://huggingface.co/settings/tokens>, then `huggingface-cli login` or set
`$HF_TOKEN`). Flags must precede the positional arguments.

```bash
# Upload a folder (recursively) in a single commit; large files go through Git LFS.
hftools upload --message "add weights" owner/model ./local_dir

# Upload one file, optionally renaming it in the repository.
hftools upload --path-in-repo config.json owner/model ./config.json

# Preview what would be sent without contacting the Hub.
hftools upload --dry-run owner/model ./local_dir

# Create the repository first if it does not exist yet.
hftools upload --create --private owner/model ./local_dir

# Manage repositories and tags.
hftools repo create --private owner/model
hftools repo delete owner/model --yes           # permanent; --yes is required
hftools tag create --message "release" owner/model v1.0
hftools tag list owner/model
hftools tag delete owner/model v1.0 --yes
```

`upload` classifies each file with the Hub's preupload endpoint, streams LFS
objects (basic or multipart) with their SHA-256, and finalizes everything in one
NDJSON commit. Small files are embedded inline; `.git` and hftools metadata
directories are skipped. A single directory argument uploads its contents under
`--path-in-repo`; a single file uploads to `--path-in-repo` (or its basename);
multiple file arguments each land at `--path-in-repo/<basename>`. Destructive
operations (`repo delete`, `tag delete`) require `--yes`.

## Scan checkpoints for unsafe code

```bash
hftools scan ./owner_model            # scan a directory of downloads
hftools scan pytorch_model.bin        # scan one file
```

`scan` statically walks the pickle opcode stream of `.bin`/`.pt`/`.pth`/`.ckpt`/
`.pkl` files (including torch zip archives) **without unpickling them**, and
flags the import references that enable code execution on load (`os`,
`subprocess`, `builtins.eval`, …). It exits non-zero when a critical import is
found. This is a heuristic aid, not a guarantee — treat unknown checkpoints as
untrusted.

## Provenance signatures

Hashing proves a download is intact; a signature proves who produced it — useful
when a repository is carried across an air gap.

hftools keeps a per-user signing identity under `~/.hftools` (override with
`HFTOOLS_HOME`): an ed25519 private key (`signing.key`, mode 0600), its public
key (`signing.pub`), and a `config.yaml` holding the signer label and a registry
of trusted public keys.

```bash
# Signer — the first `sign` creates ~/.hftools automatically:
hftools sign --output ./owner_model --signer you@example.com
hftools key show                       # print your public key + fingerprint
hftools key export --out mykey.pem     # public key to distribute out-of-band

# Recipient — trust the signer's key once, by name:
hftools key trust alice <hex-or-key-file>
hftools verify-sig --output ./owner_model          # auto-recognizes trusted keys
hftools verify-sig --output ./owner_model --pubkey alice   # or pin explicitly
```

`sign` signs the repository's content-addressed `.sha256` manifest with ed25519
and stores the detached signature in `.metadata/signature.json` and `.sha256.sig`
(the public key travels inside the signature). Without an explicit `--key`, it
uses the `~/.hftools` identity, creating it on first run.

`verify-sig` always reports the signer's public key and its SHA-256 fingerprint.
When the key matches one in your `config.yaml` trusted registry (or a `--pubkey`
name/hex/PEM/file you supply), it proves **provenance**. With no match it still
proves the content is unchanged since signing (**integrity**) and shows the
fingerprint so you can decide whether to `hftools key trust` it.

### Sign automatically

Turn on `auto_sign` and every `download`, `dataset`, `space`, `batch`, and
`verify`/`verify-batch` signs the repository with your identity the moment its
`.sha256` manifest is written — no separate `sign` step:

```bash
hftools key init --signer you@example.com --auto-sign   # sets config.yaml auto_sign: true
hftools batch --list models.txt                          # each repo is signed as it lands
hftools verify-batch --root ./repos                       # re-sign on a clean verify
hftools download owner/model --sign=false                 # opt out for one run
```

`--sign` / `--sign=false` overrides the config default per run. On first use the
identity (key + `config.yaml`) is created automatically.

Manage the identity with `hftools key`: `init`, `show`, `export`, `trust`,
`untrust`, `list`, `path`.

## Storage and maintenance

```bash
hftools du --root ./repos --by-type          # disk usage per repo / extension
hftools gc --root ./repos --tmp --orphans    # reclaim (dry run; add --yes)
hftools dedup --root ./repos --yes           # hardlink identical files across repos
hftools cache-gc --cache ~/.cache/huggingface/hub   # drop unreferenced HF-cache blobs
hftools repair --output ./owner_model        # deep-verify and re-fetch bad files
hftools watch --interval 600 owner/model     # periodically pull upstream changes
hftools doctor                               # environment / network / filesystem check
```

`gc`, `dedup`, and `cache-gc` are dry runs by default and delete only with
`--yes`. `dedup` links byte-identical files (matched by recorded SHA-256) so a
model family sharing tokenizers/configs stores one copy; cross-filesystem pairs
are skipped.

## Shell completion

```bash
hftools completion bash > /etc/bash_completion.d/hftools
hftools completion zsh  > "${fpath[1]}/_hftools"
hftools completion fish > ~/.config/fish/completions/hftools.fish
```

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
