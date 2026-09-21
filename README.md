# createapt-go

A lightweight manager for apt (Debian/Ubuntu) repositories. It replaces the most
common, cumbersome `apt-ftparchive`/`reprepro` workflows with a single tool that
works against local disk **and** remote storage, and updates a live repository
in place while transferring as little data as possible.

The headline use case:

```sh
createapt-go add <repo> <deb>...
```

uploads the packages and adds them to the live repository indexes, creating the
repository if it does not yet exist.

## Why

- **Native Go engine.** `.deb` archives are opened and the `Packages`,
  `Sources` and `Release` documents are generated in pure Go — no dependency on
  `dpkg`, `apt-ftparchive` or `reprepro` at runtime, locally or remotely. Not
  even the compression is shelled out: gzip, xz, zstd and bzip2 are all handled
  in process. This is what makes S3/GCS targets and truly minimal transfer
  possible.
- **Minimal transfer.** Updating a repository fetches only the (small) existing
  indexes, regenerates them locally, and uploads only new packages plus the new
  indexes. Existing packages are **never downloaded**.
- **Remote validation.** A package already present in the repository is
  validated by checksum without transferring it: over SSH the checksum is
  computed remotely (`sha256sum`), and on S3/GCS it is read from object metadata
  that the tool records on upload.
- **Server-side relocation.** Re-adding an identical package at a new location
  (for example after changing `--pool-layout`) moves it in place instead of
  re-uploading it, when the backend supports a server-side copy (local rename,
  SSH `cp`, S3/GCS object copy).
- **Near-atomic publish.** Every index is published under its own checksum as
  well as its plain name (`Acquire-By-Hash`), and the `Release` is swapped last,
  so apt clients never observe a torn repository. Superseded indexes **and any
  package file the new indexes no longer reference** (replaced, pruned, removed,
  or relocated) are then garbage-collected — the publish diffs the old and new
  metadata, so orphaned files never accumulate.

### Why by-hash matters here

An apt index lives at a fixed path (`main/binary-amd64/Packages`), unlike
rpm-md's checksum-named files. A client that reads the `Release`, then fetches
that path a moment after a publish, would get an index the `Release` does not
describe — which apt reports as `Hash Sum mismatch` and cannot recover from.

`Acquire-By-Hash: yes` fixes this: the same index is also written to
`by-hash/SHA256/<digest>`, and apt fetches it there. Those paths are never
reused by a later publish, so a client always reads an index that matches the
`Release` it came from. This is on by default (`--by-hash=false` to turn it
off). Superseded by-hash copies are deleted at the end of the publish, so a
client that waits long enough between the two fetches gets a 404 — which apt
retries — rather than a mismatch, which it does not.

### Compatibility targets

`--target` selects defaults appropriate for the oldest client you intend to
serve. Explicit `--compression` and `--hashes` always override the profile.

| `--target` | index compression | Release hashes |
| --- | --- | --- |
| `legacy` (aliases `debian8`, `jessie`, `ubuntu16.04`, `xenial`, …) | gzip | MD5Sum, SHA1, SHA256 |
| `modern` (`debian11`, `bullseye`, `ubuntu20.04`, `focal`, …) | gzip, xz | MD5Sum, SHA256 |
| `zstd` (`debian12`, `bookworm`, `ubuntu22.04`, `jammy`, `noble`, …) | gzip, xz, zstd | SHA256 |

The mapping is dictated by what each release's apt can actually read:

- **apt before 1.1** (Debian 8, Ubuntu 14.04/16.04) does not read `.xz` indexes
  reliably and still wants an `MD5Sum` block.
- **apt 1.1–2.2** (Debian 9–11, Ubuntu 18.04–20.04) reads `.xz`, which is
  markedly smaller than gzip for an index.
- **apt 2.3+** (Debian 12+, Ubuntu 22.04+) also reads `.zst`, which decompresses
  far faster for about the same size.

The uncompressed index is always written as well, so there is no profile in
which a client is left with nothing it can read. `SHA256` is always included in
the `Release`: apt requires it, and rejects `SHA1` as too weak to stand alone.

### There is no per-package signing

Debian has no per-package signature that apt checks. A `.deb` is trusted because
the signed `Release` vouches for the `Packages` index, which records the
package's SHA256. Signing the `Release` is therefore what makes every package in
the repository trusted, and `createapt-go` signs exactly that — writing both
`InRelease` (inline-signed) and `Release.gpg` (detached) so every client finds a
form it understands.

(`debsigs` exists, but apt ignores it without a `debsig-verify` policy that
almost nobody deploys. It is deliberately not implemented.)

### Signing is the default

Because the `Release` signature is the only thing that makes a repository
trustworthy, `createapt-go` signs by default. Name a key with `--gpg-key` or
`--gpg-key-id` and the `Release` is signed; there is no `--sign-release` to
remember.

A publishing command that names no key is **refused**, rather than quietly
producing a repository apt will not accept:

```
$ createapt-go add /srv/repo dist/*.deb
error: refusing to publish an unsigned repository: no signing key was given.
Pass --gpg-key <file> or --gpg-key-id <id> to sign the Release, or
--no-sign-release to publish without a signature
```

`--no-sign-release` publishes anyway and warns each time what that costs: every
client has to mark the source `[trusted=yes]` (or run `apt-get update
--allow-insecure-repositories`), and nobody can tell the packages came from you.
The choice is recorded in `createapt-go.json`, so later updates to that
repository do not need the flag again — they still print the warning.

### Signing unattended (CI)

Only `--gpg-key-id` can reach a passphrase prompt: it hands the signing to the
`gpg` binary, and `gpg` hands the passphrase question to `gpg-agent`, which
launches `pinentry` on whatever terminal or display it finds. `--batch` does not
stop that. On a CI runner that allocates a TTY, a missing passphrase therefore
hangs the job on a prompt nobody can answer, until the job times out.

`--gpg-no-prompt` (or `CREATEAPT_GPG_NO_PROMPT=1`) forbids the prompt: `gpg` runs
with `--no-tty` and `--pinentry-mode error`, and without `GPG_TTY`, `DISPLAY` or
`WAYLAND_DISPLAY`, so a key it cannot unlock fails the publish immediately
instead of waiting.

```sh
# Unattended publish: the passphrase comes from the environment, and a prompt
# is an error rather than a hang.
export CREATEAPT_GPG_NO_PROMPT=1
export CREATEAPT_GPG_PASSPHRASE="$SIGNING_KEY_PASSPHRASE"
createapt-go add s3://apt.example.com dist/*.deb --gpg-key-id releases@example.com
```

`--gpg-key FILE` never prompts at all: the key file is read and unlocked
in-process, so an encrypted key with no `--gpg-passphrase` is a plain error.

## Backends

The repository location's URL scheme selects the backend:

| Scheme | Example | Notes |
| --- | --- | --- |
| local | `/srv/repo` or `file:///srv/repo` | read/write |
| SSH/SFTP | `sftp://user@host/srv/repo` | uses the ssh-agent and `~/.ssh/known_hosts`; remote `sha256sum` validation. The host key is verified against `~/.ssh/known_hosts`, and the connection fails if that file cannot be read or does not list the host — pass `--insecure-ignore-host-key` to connect without verification |
| S3 | `s3://bucket/prefix` | standard AWS credential chain; `--profile`/`--region` (or `AWS_PROFILE`/`AWS_REGION`); `AWS_ENDPOINT_URL` for S3-compatible stores. The bucket's region is detected automatically, and a wrong `--region` is corrected with a warning naming the right endpoint. MFA-protected assume-role profiles are prompted for on stdin, and the resulting session credentials are cached (see below) so later commands do not ask for another code |
| GCS | `gs://bucket/prefix` | application-default credentials |
| HTTP(S) | `https://host/repo/` | **read-only** (`list`, `verify`, `check`, and as a `copy` source) |

### Cached AWS session credentials

An MFA code cannot be used twice, so a profile with `role_arn` + `mfa_serial`
would otherwise need a new code (and a new 30-second wait) for every command.
When the active profile assumes a role or names an MFA device, the session
credentials that come back are written to
`${XDG_CACHE_HOME:-~/.cache}/createapt-go/aws/<hash>.json` (mode 0600, one file
per profile/role/MFA-device combination) and reused until five minutes before
they expire. Only temporary credentials are stored; long-term access keys are
never copied out of `~/.aws/credentials`.

Set `CREATEAPT_AWS_CACHE` to another directory to move the cache, or to `off` to
disable it. Deleting the files forces a fresh prompt.

## Repository layout

A repository has three dimensions — suite, component and architecture — where a
yum repository is flat:

```
<repo>/
  dists/<suite>/
    Release            InRelease            Release.gpg
    <component>/binary-<arch>/Packages{,.gz,.xz}  Release  by-hash/…
    <component>/source/Sources{,.gz,.xz}          Release  by-hash/…
  pool/<component>/<prefix>/<source>/…
  createapt-go.json
```

`--suite` (default `stable`) and `--component` (default `main`) select where
packages land; they are recorded in `createapt-go.json`, so later commands that
omit them keep updating the same place rather than quietly creating a second
suite beside the first.

An `Architecture: all` package is written into **every** architecture's
`Packages` index rather than into a `binary-all` index of its own. That is what
every apt version understands, and it costs one repeated stanza of a few hundred
bytes. `all` therefore does not appear in the `Release`'s `Architectures` line
unless it is the only architecture present.

## Commands

```
createapt-go add     <repo> <deb|dsc|dir>... # upload packages (dirs scanned recursively) and update the indexes
createapt-go remove  <repo> <name>...  # remove packages (by name; --arch/--version)
createapt-go rebuild <repo>            # reconcile an existing suite against updated options
createapt-go copy    <src> <dst>       # copy/mirror a repository to another location
createapt-go create  <repo>            # initialize an empty suite (records config)
createapt-go list    <repo>            # list packages
createapt-go verify  <repo>            # check the published files match the indexes
createapt-go check   <repo>            # deep-validate indexes + packages (levels)
```

Useful flags (global unless noted):

```
--target legacy|modern|zstd   set compression + hash defaults for a release
                              (aliases: debian12, bookworm, jammy, noble, ...)
--suite NAME              suite (dists/ subdirectory) to operate on (default stable)
--component NAME          component to place added packages in (default main)
--dry-run                 show what would change without transferring anything
--force                   upload every staged file, overwriting the remote copy
                          unconditionally
--compression LIST        index compression: none,gzip,xz,zstd (default gzip,xz;
                          the uncompressed form is always written)
--hashes LIST             Release/index hashes: md5,sha1,sha256 (default md5,sha256;
                          sha256 is always included)
--pool-layout             use pool/<component>/<prefix>/<source>/ (default true;
                          false places packages flat under pool/<component>/)
--by-hash                 publish indexes under their checksums too (default true)
--validity DURATION       set Release Valid-Until this long after its Date (default:
                          never expires)
--origin / --label / --codename / --release-version
                          descriptive fields recorded in the Release file
--profile NAME            AWS named profile for S3 (sets AWS_PROFILE)
--region NAME             AWS region for S3 (sets AWS_REGION); the bucket's own
                          region wins if it differs, with a warning
--prune-older             (add/rebuild/copy) drop older versions of the same name+arch
--prune-break-deps        (add/rebuild/copy) allow --prune-older to drop a version even
                          when another package depends on that specific version
                          (default: keep it and warn)
--repo-name NAME          (create/copy) human-readable repo name recorded in the config
--repo-url URL            (create/copy) public base URL end users fetch from, recorded
                          in the config

A package file that the updated indexes no longer reference is always removed on
publish, so there is no flag to opt into blob cleanup.

--sign-release            GPG-sign the Release (writes InRelease and Release.gpg);
                          on by default — name a key and it is used
--no-sign-release         publish without signing the Release (warns on every publish)
--verify-sigs             require a trusted signature on a repository being read
--skip-verify             do not check the Release signature even when a key is available
--keyring FILE            public keyring to verify against (repeatable)
--gpg-key FILE            private key file (signing)
--gpg-key-id ID           key id/uid from the local keyring (signing)
--gpg-passphrase / $CREATEAPT_GPG_PASSPHRASE
--gpg-no-prompt / $CREATEAPT_GPG_NO_PROMPT
                          never let gpg ask for a key passphrase; fail instead of
                          prompting (use in CI, where a prompt hangs the job)
```

## Examples

```sh
# Publish two packages to an SSH host, signing the Release with a keyring key.
createapt-go add sftp://build@mirror/srv/repo \
    dist/*.deb --suite bookworm --gpg-key-id releases@example.com

# Add to an S3 bucket (packages land in pool/main/ by default), signing the Release.
createapt-go add s3://my-bucket/apt dist/*.deb \
    --suite jammy --gpg-key ./signing.key

# Add every package under a directory tree (subdirectories are scanned recursively).
createapt-go add /srv/repo dist/ --gpg-key-id releases@example.com

# Publish a scratch repository with no signature at all (apt clients will need
# [trusted=yes]).
createapt-go add /srv/scratch dist/*.deb --no-sign-release

# Add a source package alongside its binaries. The .dsc's tarballs are uploaded
# with it, and are checked against the checksums the .dsc records first.
createapt-go add /srv/repo dist/*.deb dist/*.dsc

# Preview an update without changing anything.
createapt-go add /srv/repo new.deb --dry-run
```

After a publish, clients subscribe with the line `create` prints (given
`--repo-url`):

```
deb https://downloads.example.com/apt bookworm main
```

## Rebuilding a repository

`rebuild` loads an existing suite, applies updated options, reconciles the new
state, and republishes it. It always prints a before/after summary and asks for
confirmation before making any change (`--yes` to skip the prompt, or
`--dry-run` to only report). Recorded config defaults and any overriding flags
(`--pool-layout`, `--gpg-key`, `--target`, …) apply exactly as they do to
`add`/`remove`.

### Indexes are regenerated from the packages when they are local

If a package file was replaced under the same name — a rebuilt `.deb` published
over the old one — the index still describes the build it superseded, and
`verify` reports a checksum mismatch (users see `Hash Sum mismatch`). `rebuild`
fixes that by re-reading every `.deb` and regenerating its stanza (checksum,
size, dependencies) from the file itself.

Re-reading local files is free, so it is the default. When the packages live on
remote storage it would mean downloading the whole repository, so it is off
unless you ask:

```sh
# Local: the packages are re-read automatically.
createapt-go rebuild /srv/repo --yes

# Remote: opt in, and pay for the download.
createapt-go rebuild s3://my-bucket/apt --from-packages --yes
```

When `rebuild` is *not* re-reading the packages it says so, because it is then
republishing indexes it has not checked:

```
warning: the indexes are being regenerated from the published index, not re-read
from the package files, so anything the index gets wrong about a package
(checksum, size, dependencies) stays wrong. Pass --from-packages to re-read
them, which downloads 20 package(s) (~583.5 MiB).
```

Use `--from-packages=false` to suppress the re-read on a local repository.

Source packages are not re-read: a `.dsc` carries its own checksums for its
tarballs, so the `Sources` index only restates what the `.dsc` already asserts.
A source package whose files changed underneath it is caught by `verify`.

Three situations are reported rather than silently resolved:

- **Two index entries naming one file** (a stale record left beside the current
  one by a bad publish) collapse onto the entry that matches the file. The count
  is reported as `N duplicate record(s) dropped`; no file is deleted, since
  another entry still names it.
- **A referenced file that is missing**, and **a file that cannot be read as a
  Debian package**: the entry is left exactly as published and a warning names
  it.
- **Two locations holding byte-identical packages** cannot both be indexed, and
  dropping either would leave its file unreferenced and due for deletion. Both
  entries are left exactly as published and a warning names the two locations.

Apart from the re-read, rebuild is metadata-only and never downloads packages:

```sh
# Drop every superseded version, keeping only the newest of each name+arch.
createapt-go rebuild /srv/repo --prune-older

# Move every package into the conventional pool tree (relocated server-side, no
# re-upload where the backend supports it).
createapt-go rebuild s3://my-bucket/apt --pool-layout

# Start publishing xz and zstd indexes as well as gzip.
createapt-go rebuild /srv/repo --target bookworm --yes
```

Opt-in extras:

```
--from-packages                re-read every .deb and regenerate its stanza from the
                               file (default: on for local packages, off for remote)
--remove-unreferenced-packages delete pool files the indexes no longer reference
--remove-stale-metadata        delete index files under dists/ no longer in use
-y, --yes                      skip the confirmation prompt
```

`--remove-unreferenced-packages` / `--remove-stale-metadata` require a backend
that can enumerate its contents (local, SSH/SFTP, S3, GCS). They are unavailable
on the read-only HTTP backend.

## Copying a repository (`copy`)

`copy` reads a repository from one location and writes it to another. Source and
destination may each be any supported backend, so it mirrors between local disk,
SSH/SFTP, S3, GCS, and a read-only HTTP source.

```sh
# Mirror a public repository onto S3, exactly as it stands.
createapt-go copy https://deb.example.com/apt s3://my-bucket/apt --suite bookworm
```

### Exact by default

By default the copy is **exact**: every file is transferred byte for byte, so
the destination is a replica and the `InRelease`/`Release.gpg` it carries stay
valid. Nothing is regenerated and no timestamps change.

Everything the copy touches is verified as it passes through: each index and
each package file is checked against the checksum the metadata records, and a
mismatch aborts the copy. A file already present at the destination with
matching content is not transferred again, so an interrupted copy resumes
cheaply.

Only the named suite's `dists/` tree is copied, along with the pool files its
indexes reference. Other suites sharing the same pool are left alone.

### Writing into a suite that already exists

`copy` refuses to write over an existing suite unless told which of the three
things you mean:

```
--overwrite  replace it; files at the destination the source does not have are deleted
--continue   resume an interrupted copy; fails if the destination is a different repository
--update     merge the source's packages into it (an incremental copy)
```

`--continue` establishes that it is resuming the same copy from the
`copy_source` recorded in the destination's `createapt-go.json`, and failing
that by checking that the destination holds nothing the source does not have.

### Selecting and transforming

```
--latest-only              copy only the newest version of each name+arch
--include PATTERN          only copy packages matching these globs (repeatable)
--exclude PATTERN          do not copy packages matching these globs (repeatable)
--arch ARCH[,ARCH...]      only copy these architectures; fails if one is absent
--kinds KIND[,KIND...]     only copy these kinds: binary, source, udeb, debug
                           (aliases: debuginfo, dbgsym)
--exclude-kinds KIND...    do not copy these kinds
--to-suite NAME            publish into this suite instead of the source's
--to-component NAME        index the copied packages in this component
--rebuild-metadata         regenerate each stanza from the copied .deb
--prune-older              after copying, drop superseded versions from the destination
--prune-break-deps         let --prune-older drop a version another package depends on
--repo-name NAME           name to record in the destination's config file
--repo-url URL             public base URL of the copy, recorded in its config
--remove-unreferenced-packages  delete pool files the copied indexes do not reference
--remove-stale-metadata    delete index files no longer in use
-y, --yes                  skip the confirmation prompt
```

`--prune-older` reconciles the destination after the copy, so it also drops
versions the destination already held — that is what makes it useful with
`--update`. On a fresh copy, `--latest-only` gets the same result without
transferring the versions that are about to be dropped.

`--include`/`--exclude` patterns are shell globs matched against a package's
name and its `name_version` and `name_version_arch` forms, so `hello`,
`hello_2.10*` and `libfoo_1.2.0-1_amd64` all select what you would expect.
`--arch` keeps `Architecture: all` packages alongside the architecture you ask
for, because a repository for one architecture still needs them; name `source`
to keep source packages.

A **debug** package is one named `*-dbgsym`/`*-dbg` or filed under
`Section: debug`. It is classified as debug rather than binary, so
`--exclude-kinds debug` does what it says without also needing `binary` listed.

Any of these options makes the copy **rebuild** the destination's indexes rather
than replicate them, and the run reports which one did.

### Verification and signing

If the source repository records a signing key in its `createapt-go.json`, or
you supply one with `--keyring`, the copy verifies the source's `InRelease`
(preferred, because it is one document, so what was verified is exactly what was
read) or its detached `Release.gpg`. Verification is automatic; `--verify-sigs`
makes a missing key or an unsigned source an error rather than a warning, and
`--skip-verify` turns it off.

Rebuilt indexes are a different document from the one the source signed, so a
**signed** source may only be transformed if the copy gets a signature of its
own:

```sh
# Take only the newest amd64 build of each package, no debug or source packages,
# and sign the rebuilt indexes with our own key.
createapt-go copy /srv/upstream /srv/mirror \
    --latest-only --arch amd64 --exclude-kinds debug,source \
    --gpg-key-id releases@example.com
```

Without a key of its own that copy is refused, because it would otherwise
publish indexes alongside a signature that no longer matches them. An unsigned
source has no such constraint.

### Notes

- The copy holds only one file on local disk at a time, so a mirror of any size
  needs no more room than its largest package.
- `--dry-run` reports the whole plan without fetching or writing anything.
- A source that cannot enumerate its contents (plain HTTP) contributes only the
  files its metadata references; anything else it stores is invisible and is
  reported as such. The by-hash copies are still found, because their paths
  follow from the checksums the `Release` lists.
- `--overwrite` deletes destination files the source does not have, so it asks
  for confirmation first unless `--yes` or `--dry-run` is given.

## Repository config file

`create`, `add`, `remove`, `rebuild` and `copy` maintain a small
`createapt-go.json` file at the repository root (alongside `dists/` and
`pool/`). It records the repository's identity and the choices in effect:

```json
{
  "name": "Frobulator Beta Bookworm",
  "baseurl": "https://downloads.example.com/apt",
  "suite": "bookworm",
  "component": "main",
  "target": "bookworm",
  "pool_layout": true,
  "by_hash": true,
  "origin": "Frobulator",
  "label": "Frobulator Beta",
  "codename": "bookworm",
  "sign_release": true,
  "gpg_key_id": "FF652CAEF8C583AB827F219E992DF1A087059154",
  "copy_source": "https://downloads.example.com/apt"
}
```

- `name` / `baseurl` are set with `--repo-name` / `--repo-url` on `create` and
  preserved across later runs.
- `suite` / `component` record where packages were last published, so a later
  `add` that omits them updates the same suite instead of creating a second one.
- `target` records the compatibility profile in effect. A later run that omits
  `--target` re-applies that profile's defaults (compression, hash set, and any
  future target-derived settings), so the repository stays consistent with the
  releases it was built for.
- `pool_layout` / `by_hash` record the publish layout, so later uploads land in
  the same place and indexes keep being published the same way.
- `origin`, `label`, `codename` are the `Release` fields, preserved across
  publishes.
- `copy_source` is written by `copy` and records where the repository was copied
  from, so a later `copy --continue` can tell that it is resuming the same copy
  rather than overwriting an unrelated repository.
- `gpg_key_id` is also what `copy` verifies a source repository against when no
  `--keyring` is supplied: it exports that key from the local GnuPG keyring and
  checks the `Release` signature with it.
- `gpg_key_id` is always stored as the key's **fingerprint**, resolved from
  whichever of `--gpg-key`/`--gpg-key-id` was supplied, so it is portable across
  machines.
- The recorded settings act as **defaults**: a later run that omits the relevant
  flags re-uses them (re-signs the `Release` with the same key, keeps the same
  suite and target profile). Pass the flags explicitly to override — an explicit
  CLI `--target`, `--suite` or `--hashes` wins over the recorded one.
  Config-driven signing resolves the key from the local GnuPG keyring by
  fingerprint, so that key must be importable there.

The file is plain JSON and is **not** part of the apt metadata — apt ignores it
and it is never referenced by a `Release` file.

```sh
# Initialize a repository and record its metadata + signing defaults.
createapt-go create /srv/repo \
    --suite bookworm --component main \
    --repo-name "Frobulator Beta Bookworm" \
    --repo-url  "https://downloads.example.com/apt" \
    --origin Frobulator --label "Frobulator Beta" \
    --gpg-key-id releases@example.com

# Later updates re-sign automatically from the recorded config.
createapt-go add /srv/repo dist/*.deb
```

## Verifying a repository (`verify`)

`verify` checks that every file the indexes reference is present with the size
and checksum recorded, and that the repository's intra-repository dependencies
are satisfied. It modifies nothing.

```sh
createapt-go verify /srv/repo
# OK: 42 package(s) present with the expected size (42 file(s)); checksums:
# 42 verified from content; dependencies satisfied
```

How far the checksum check gets depends on what the backend can prove without
transferring the object:

| Backend | Default checksum check |
|---------|------------------------|
| local disk | Computed from the stored file. |
| SSH/SFTP | Computed by `sha256sum` on the remote host, so the package never crosses the network. |
| S3, GCS | The checksum **recorded when the object was uploaded**. This catches indexes and objects disagreeing, but not an object whose content changed afterwards. |
| HTTP(S) | None: no checksum is available without downloading. |

`--checksums` proves every file's checksum from its content regardless,
downloading each one the backend cannot hash in place:

```sh
# Prove the contents of an S3 repository, not just its recorded checksums.
createapt-go verify s3://my-bucket/apt --checksums
```

The summary always states which of the three applied, so an unproved checksum is
never reported as though it had been verified, and a run with anything left
unconfirmed says so and points at `--checksums`.

```
--checksums          hash every file's content, downloading where necessary
--concurrency N      packages to check in parallel (default 6)
```

`verify` reports the estimated download volume before starting a `--checksums`
run against a backend that cannot hash in place. Nothing is written to disk:
each file is hashed as it streams past.

For a deeper sweep that also validates the index files themselves and opens each
package archive, use `check --level fetch` below.

## Validating a repository (`check`)

`verify` is the quick consistency check: it confirms every indexed package's
file exists and, where the backend can hash remotely, that its checksum matches.
`check` is the deeper validator. It reads the suite's `Release`, checks every
index it covers against the size and checksums recorded there, parses those
indexes, and confirms the packages they reference really exist with the size and
checksum claimed.

Both commands also validate the **dependency graph**: every `Depends`/
`Pre-Depends` that the repository could satisfy from its own packages (some
package provides the name) must be met by an available version. A dependency on
a name no package in the repository provides — `libc6`, `dpkg` — is external and
is left to the installing system. This catches a prune or removal that dropped a
version another package still needs (for example `pinner` depending on
`pinned (= 1.0-1)` after `pinned 1.0-1` was pruned). `rebuild` reports the same
broken dependencies in its plan, and `--prune-older` will not drop a version
that another package pins unless `--prune-break-deps` is given.

`check` exists to catch repositories where a published package file has diverged
from what was indexed — for example when one `.deb` is published over another
without regenerating the metadata. End users see this as:

```
Failed to fetch .../pool/main/h/hello/hello_2.10-3_amd64.deb
  Hash Sum mismatch
```

`check` runs against **any** backend — local disk, SSH/SFTP, S3, GCS, or
HTTP(S) — so the same validation covers a repository at rest as well as one
published live.

### Levels

Validation is layered; each `--level` includes everything the previous one does:

| `--level`  | Checks |
|------------|--------|
| `metadata` | The suite's `InRelease`/`Release` is present and parseable (and not expired), and every index it references reads back with the correct size and checksums, decompresses, and parses. |
| `head`     | …plus every package exists with the size the index claims (and, when the backend can checksum without transferring the file — S3, GCS, SFTP — the correct checksum). |
| `fetch`    | …plus every package is downloaded and its size **and** checksum verified from its content, then its archive is opened and its control stanza parsed. |

The default level is `head`.

The archive check needs no tooling installed: the `ar` container and the
compressed control tarball are walked in process, which exercises the same
structure a client's `dpkg` would read.

### Version selection

`--versions` controls whether all versions of a package are checked or only the
newest (`all` / `latest`). The default depends on the level: `latest` for
`fetch` (so a full download stays cheap) and `all` for `metadata`/`head`.
"Latest" uses the dpkg-faithful version comparison (including the `~` ordering
and the letters-before-punctuation rule), so `dpkg` is **not** required to
determine it.

### Input

The positional argument (or `--sources-file`) may be a repository location — a
local path, or a `file://`, `sftp://`, `s3://`, `gs://` or `http(s)://` URL — or
a path/URL to a sources file (ending in `.list` or `.sources`), in which case
every entry it names is checked as its own target. Both the one-line
`sources.list` format and the deb822 `.sources` format are read, including their
`[arch=…]`/`Architectures` options and `Enabled: no`.

With no `--suite`, a repository location is checked at the suite its
`createapt-go.json` records, falling back to `stable`.

### Examples

```sh
# HEAD-check every package in a live repository:
createapt-go check https://deb.example.com/apt --suite bookworm

# Check every entry a sources file names:
createapt-go check /etc/apt/sources.list.d/example.sources

# Fully download and verify a local repository:
createapt-go check --level fetch /srv/repo

# Checksum-verify just two packages on S3 without a full sweep:
createapt-go check --level head --packages hello,libfoo s3://my-bucket/apt

# Only validate that a GCS repository's indexes are internally consistent:
createapt-go check --level metadata gs://my-bucket/apt
```

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--sources-file` | – | Path/URL to a `.list` or `.sources` file (alternative to the positional arg). |
| `--level` | `head` | `metadata` \| `head` \| `fetch`. |
| `--suite` | recorded, else `stable` | Comma-separated suites to check. |
| `--component` | all | Comma-separated components to check. |
| `--arch` | host arch | Comma-separated architectures (e.g. `amd64,arm64`). `any` checks every architecture in the indexes; name `source` to include source packages. |
| `--packages` | all | Comma-separated package names to check. |
| `--versions` | level-dependent | `latest` \| `all`. |
| `--concurrency` | `6` | Parallel package checks. |
| `--timeout` | `60s` | Per-operation timeout (`0` to disable). |
| `--verbose` | `false` | Print a line for every check, not just issues. |

### Exit codes

`check` exits `0` when all checks pass, and non-zero when at least one check
fails (the failure details are printed first). Other commands exit non-zero on
any error.

## Library

The CLI is a thin wrapper over reusable packages:

- `pkg/aptdata` — the deb822 data model, `Packages`/`Sources`/`Release`
  documents, dpkg version comparison, the dependency graph, index compression,
  and the repository layout rules.
- `pkg/debmeta` — extract repository metadata from a `.deb` (pure-Go `ar` and
  control-tarball reader) or a `.dsc`.
- `pkg/backend` — storage abstraction (local/sftp/s3/gcs/http) with optional
  `RemoteHasher`, `Lister`, `Copier` and `FileStore` capabilities.
- `pkg/repo` — load, mutate and publish a suite (`Open`/`Add`/`Remove`/
  `Commit`); `OpenWith` accepts a custom backend.
- `pkg/repocheck` — layered validation (`metadata`/`head`/`fetch`) of a
  repository over any backend, including sources-file handling
  (`repocheck.Run`).
- `pkg/sign` — `Release` signing (detached and clearsigned), signature
  verification, and signing-key fingerprint resolution.
- `pkg/repoconfig` — load/save the `createapt-go.json` repository config file.

The zero value of `repo.Options` produces the recommended repository: the
conventional pool tree and by-hash publishing are on unless you set `FlatPool`
or `NoByHash`, so a library caller cannot lose the near-atomic publish by
forgetting a field.

## Testing

```sh
go test ./...        # fast unit + golden tests
make test-e2e        # full end-to-end suite (servers/containers; slower)
```

The unit tests build sample packages and compare the extracted control stanza
field-by-field against `dpkg-deb --field`, cross-check the version ordering
against `dpkg --compare-versions`, assert the minimal-transfer behavior (nothing
is uploaded when adding a package that is already published), and round-trip
`Release` signing and verification through both the native OpenPGP path and the
`gpg` binary. Tests needing `dpkg-deb` or `gpg` skip themselves when those are
absent. Regenerate the fixtures with `reference/gen.sh` (needs `dpkg-deb`).

The **end-to-end suite** (`test/e2e/`, behind the `e2e` build tag) builds the
CLI and drives it the way a user would, then reads the repositories it produces
back with a **real apt** — which is the only authority on whether the metadata
is correct. It covers the signed and unsigned paths, source packages, the
add/prune/remove lifecycle, `copy` (exact and transforming), `rebuild` repairing
a rewritten package, and the `--target` profiles. The same publish/verify/check
cycle also runs against a freshly started MinIO instance for S3 and a local
`sshd` for SFTP. Everything optional skips itself, so the suite is useful with
only `dpkg-deb` installed. Run it with `make test-e2e` or
`go test -tags e2e ./test/e2e/...`; see [`test/e2e/README.md`](test/e2e/README.md)
for the full coverage list and the optional dependencies.
