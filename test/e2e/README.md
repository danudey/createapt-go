# End-to-end tests

These tests build the `createapt-go` binary and drive it the way a user would,
then read the repositories it produces back with a **real apt**. That last part
is the point: apt is the only authority on whether the metadata this tool writes
is correct, and nothing short of pointing it at a generated repository proves
it.

```sh
make test-e2e
# or
go test -tags e2e ./test/e2e/... -timeout 600s
```

They are behind the `e2e` build tag, so `go test ./...` does not run them.

## Prerequisites

Everything optional **skips itself** rather than failing, so the suite is useful
on a machine with only `dpkg-deb` and complete on one with everything.

| Needed for | Dependency | Without it |
| --- | --- | --- |
| Building the sample packages | `dpkg-deb` (run `reference/gen.sh` once) | every test skips |
| Reading repositories back | `apt-get`, `apt-cache` | the apt assertions skip |
| Signing scenarios | `gpg` | the signing tests skip |
| The S3 backend | `minio` on `$PATH` | the `s3` subtest is skipped |
| The SFTP backend | `sshd`, `ssh-keygen`, and a running `ssh-agent` holding a key | the `sftp` subtest is skipped |

Nothing needs root, and nothing touches the host's apt configuration, keyring or
dpkg database: each test gets an isolated apt tree under `t.TempDir()` with its
own `apt.conf`, sources, trusted keys, lists and (empty) dpkg status.

The services the suite starts — MinIO and `sshd` — bind to `127.0.0.1` on a
kernel-assigned free port, write only into a temporary directory, and are killed
when the test that started them ends.

## What is covered

**`scenarios_test.go`** — the behaviors the README promises, each verified
against a real apt where apt can see them:

- An unsigned repository is readable with `[trusted=yes]`, and the packages it
  advertises download with matching checksums.
- A signed repository is **accepted** when apt trusts the key and **refused**
  when it does not — both directions, because only checking the first would
  pass on a repository that was never signed at all.
- Source packages appear to `apt-cache showsrc` through a `deb-src` line.
- The everyday lifecycle: add, add again (transferring nothing), add a newer
  version, `--prune-older`, remove — with apt's candidate version checked after
  each step, and the recorded suite reused without being re-specified.
- `--prune-older` protects a version another package pins, and
  `--prune-break-deps` drops it and makes `verify` fail afterwards.
- A package rewritten under the indexed name is caught by `verify` and by
  `check`, and repaired by `rebuild`.
- `copy` produces a byte-identical replica whose original signature still
  verifies; a transforming copy of a signed source is refused without a key of
  its own and succeeds with one.
- `copy` from a read-only HTTP source, which cannot enumerate its own contents
  and must say so.
- `--target` profiles change which compressed indexes and which Release hash
  blocks are written, and the result is still readable.
- `--dry-run` writes nothing at all.

**`backends_test.go`** — the same publish/verify/check cycle against every
storage backend that is available (`local`, `s3`, `sftp`), which is what proves
the storage abstraction rather than just the local path produces a working
repository. The minimal-transfer guarantee is asserted per backend, because each
one establishes "already present" differently: a recorded checksum on S3, a
remote `sha256sum` over SFTP, a local hash on disk.

**Not covered here:** GCS, which has no usable local emulator on the same terms
as MinIO, and cross-distribution client testing in containers. Both are exercised
by hand before a release.
