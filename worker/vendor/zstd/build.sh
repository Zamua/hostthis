#!/bin/sh
# Rebuilds zd.wasm, libzstd's decompressor and zd.c as a wasm32-wasi reactor,
# from a pinned zstd release with a pinned zig, and records its sha256 in
# zd.wasm.sha256. The result is byte-identical across rebuilds, so `git diff`
# after running this shows whether the committed module matches its source.
#
# Needs curl, tar, shasum or sha256sum, and nix (for zig). The module's one
# import, wasi_snapshot_preview1.random_get, comes from wasi-libc's allocator
# and is stubbed by src/public/zstd.js.
set -eu

ZSTD_VERSION=1.5.7
ZSTD_SHA256=eb33e51f49a15e023950cd7825ca74a4a2b43db8354825ac24fc1b7ee09e6fa3
ZSTD_URL=https://github.com/facebook/zstd/releases/download/v$ZSTD_VERSION/zstd-$ZSTD_VERSION.tar.gz
# A nixpkgs revision whose zig is 0.16.0.
NIXPKGS=github:NixOS/nixpkgs/7a0f122f5090cf4c2ade2a13a0e229d4e19ba71f
ZIG_VERSION=0.16.0

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

curl -fsSL "$ZSTD_URL" -o "$work/zstd.tar.gz"
got=$(sha256 "$work/zstd.tar.gz")
if [ "$got" != "$ZSTD_SHA256" ]; then
  echo "zstd-$ZSTD_VERSION.tar.gz: sha256 $got, want $ZSTD_SHA256" >&2
  exit 1
fi
tar -xzf "$work/zstd.tar.gz" -C "$work"
cp "$here/zd.c" "$work/"

cd "$work"
zig() { nix shell "$NIXPKGS#zig" -c zig "$@"; }
have=$(zig version)
if [ "$have" != "$ZIG_VERSION" ]; then
  echo "zig $have, want $ZIG_VERSION" >&2
  exit 1
fi

Z=zstd-$ZSTD_VERSION/lib
zig cc -target wasm32-wasi -mexec-model=reactor \
  -O2 -flto -DNDEBUG -DZSTD_LEGACY_SUPPORT=0 -DZSTD_NO_TRACE -DZSTD_DISABLE_ASM -DZSTD_MULTITHREAD=0 \
  -DXXH_NAMESPACE=ZSTD_ \
  -mbulk-memory -msign-ext -mnontrapping-fptoint \
  -I$Z -I$Z/common \
  $Z/common/entropy_common.c $Z/common/error_private.c $Z/common/fse_decompress.c $Z/common/zstd_common.c $Z/common/xxhash.c $Z/common/debug.c \
  $Z/decompress/zstd_decompress.c $Z/decompress/zstd_decompress_block.c $Z/decompress/zstd_ddict.c $Z/decompress/huf_decompress.c \
  zd.c -Wl,--strip-all -Wl,--initial-memory=2097152 -Wl,-z,stack-size=65536 -s -o zd.wasm

cp zd.wasm "$here/zd.wasm"
printf '%s  zd.wasm\n' "$(sha256 zd.wasm)" > "$here/zd.wasm.sha256"
cat "$here/zd.wasm.sha256"
