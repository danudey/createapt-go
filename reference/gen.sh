#!/usr/bin/env bash
#
# Build the sample packages the unit and golden tests use.
#
# Needs dpkg-deb (dpkg) and, for the source package, tar. Nothing here needs
# root: dpkg-deb --root-owner-group writes the archive with root-owned members
# without actually being root.
#
# Run from the repository root:  ./reference/gen.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
build="${here}/build"
out="${here}/packages"

command -v dpkg-deb >/dev/null || { echo "dpkg-deb is required" >&2; exit 1; }

rm -rf "${build}" "${out}"
mkdir -p "${build}" "${out}"

# ---------------------------------------------------------------------------
# hello 2.10-3, architecture all: the simplest useful package.
# ---------------------------------------------------------------------------
pkg="${build}/hello"
mkdir -p "${pkg}/DEBIAN" "${pkg}/usr/bin" "${pkg}/usr/share/doc/hello"
cat > "${pkg}/DEBIAN/control" <<'EOF'
Package: hello
Version: 2.10-3
Architecture: all
Maintainer: createapt-go tests <tests@example.com>
Installed-Size: 24
Depends: libfoo (>= 1.3.0)
Section: devel
Priority: optional
Homepage: https://example.com/hello
Description: friendly greeting program
 A minimal package used by the createapt-go test suite. It prints a
 greeting and exists only so there is something to index.
 .
 It has no other purpose.
EOF
cat > "${pkg}/usr/bin/hello" <<'EOF'
#!/bin/sh
echo "Hello, world!"
EOF
chmod 0755 "${pkg}/usr/bin/hello"
echo "sample documentation" > "${pkg}/usr/share/doc/hello/README"
dpkg-deb --root-owner-group --build "${pkg}" "${out}/hello_2.10-3_all.deb" >/dev/null

# ---------------------------------------------------------------------------
# libfoo 1.3.0-1, architecture amd64: satisfies hello's dependency, and
# exercises the lib* pool prefix (pool/main/libf/libfoo/).
# ---------------------------------------------------------------------------
pkg="${build}/libfoo"
mkdir -p "${pkg}/DEBIAN" "${pkg}/usr/lib/x86_64-linux-gnu"
cat > "${pkg}/DEBIAN/control" <<'EOF'
Package: libfoo
Source: foo
Version: 1.3.0-1
Architecture: amd64
Maintainer: createapt-go tests <tests@example.com>
Installed-Size: 16
Section: libs
Priority: optional
Multi-Arch: same
Provides: libfoo1 (= 1.3.0)
Description: foo shared library
 The shared library half of the foo project, used by the createapt-go
 test suite to exercise versioned Provides and the lib* pool prefix.
EOF
printf 'not really a shared object\n' > "${pkg}/usr/lib/x86_64-linux-gnu/libfoo.so.1"
dpkg-deb --root-owner-group --build "${pkg}" "${out}/libfoo_1.3.0-1_amd64.deb" >/dev/null

# ---------------------------------------------------------------------------
# libfoo 1.4.0-1: a newer version, so --prune-older has something to prune and
# hello's "libfoo (>= 1.3.0)" stays satisfiable across the prune.
# ---------------------------------------------------------------------------
sed -i 's/^Version: 1.3.0-1$/Version: 1.4.0-1/; s/^Provides: libfoo1 (= 1.3.0)$/Provides: libfoo1 (= 1.4.0)/' \
    "${pkg}/DEBIAN/control"
dpkg-deb --root-owner-group --build "${pkg}" "${out}/libfoo_1.4.0-1_amd64.deb" >/dev/null

# ---------------------------------------------------------------------------
# pinned 1.0-1 / 2.0-1 plus a dependant that pins the old version exactly, so
# the prune-breaks-dependencies path has a case to report.
# ---------------------------------------------------------------------------
pkg="${build}/pinned"
mkdir -p "${pkg}/DEBIAN"
for v in 1.0-1 2.0-1; do
  cat > "${pkg}/DEBIAN/control" <<EOF
Package: pinned
Version: ${v}
Architecture: amd64
Maintainer: createapt-go tests <tests@example.com>
Installed-Size: 8
Section: misc
Priority: optional
Description: package pinned by another
 Exists so the test suite can prune one version and check that a
 dependant pinning the other version is protected.
EOF
  dpkg-deb --root-owner-group --build "${pkg}" "${out}/pinned_${v}_amd64.deb" >/dev/null
done

pkg="${build}/pinner"
mkdir -p "${pkg}/DEBIAN"
cat > "${pkg}/DEBIAN/control" <<'EOF'
Package: pinner
Version: 1.0-1
Architecture: amd64
Maintainer: createapt-go tests <tests@example.com>
Installed-Size: 8
Depends: pinned (= 1.0-1)
Section: misc
Priority: optional
Description: depends on one exact version of pinned
 Exists so pruning pinned 1.0-1 is known to break something.
EOF
dpkg-deb --root-owner-group --build "${pkg}" "${out}/pinner_1.0-1_amd64.deb" >/dev/null

# ---------------------------------------------------------------------------
# A source package: foo 1.3.0-1, as a .dsc plus its tarballs. Built by hand
# rather than with dpkg-source, so the fixtures need no dpkg-dev.
# ---------------------------------------------------------------------------
src="${build}/src"
mkdir -p "${src}/foo-1.3.0"
echo "int main(void) { return 0; }" > "${src}/foo-1.3.0/foo.c"
tar -czf "${out}/foo_1.3.0.orig.tar.gz" -C "${src}" foo-1.3.0

mkdir -p "${src}/debian"
echo "3.0 (quilt)" > "${src}/debian/source-format"
tar -czf "${out}/foo_1.3.0-1.debian.tar.gz" -C "${src}" debian

# The .dsc records each tarball's size and checksums, which createapt-go
# re-derives from the files themselves and refuses to index if they disagree.
emit_checksums() {
  local field="$1" prog="$2"; shift 2
  printf '%s:\n' "${field}"
  for f in "$@"; do
    printf ' %s %s %s\n' "$(${prog} "${out}/${f}" | cut -d' ' -f1)" \
        "$(stat -c%s "${out}/${f}")" "${f}"
  done
}
{
  cat <<'EOF'
Format: 3.0 (quilt)
Source: foo
Binary: libfoo
Architecture: any
Version: 1.3.0-1
Maintainer: createapt-go tests <tests@example.com>
Standards-Version: 4.6.2
Build-Depends: debhelper-compat (= 13)
Package-List:
 libfoo deb libs optional arch=any
EOF
  emit_checksums "Checksums-Sha256" sha256sum foo_1.3.0.orig.tar.gz foo_1.3.0-1.debian.tar.gz
  emit_checksums "Files" md5sum foo_1.3.0.orig.tar.gz foo_1.3.0-1.debian.tar.gz
} > "${out}/foo_1.3.0-1.dsc"

rm -rf "${build}"
echo "wrote $(ls -1 "${out}" | wc -l) file(s) to ${out}"
ls -1 "${out}"
