#!/bin/bash
set -e

RELEASES_DIR="/var/www/nebula/releases"
CURRENT_LINK="/var/www/nebula/current"
TARBALL="/tmp/frontend.tar.gz"

VERSION=$(date +%Y%m%d-%H%M%S)
TARGET_DIR="$RELEASES_DIR/$VERSION"

echo "Deploying version: $VERSION"

mkdir -p "$RELEASES_DIR"
mkdir -p "$TARGET_DIR"
tar -xzf "$TARBALL" -C "$TARGET_DIR"

# 原子切换软链接
ln -sfn "$TARGET_DIR" "${CURRENT_LINK}_tmp"
mv -T "${CURRENT_LINK}_tmp" "$CURRENT_LINK"

# 保留最近 5 个版本
ls -td "$RELEASES_DIR"/* | tail -n +6 | xargs -r rm -rf

nginx -t
nginx -s reload

rm -f "$TARBALL"

echo "Deployed successfully: $VERSION"
echo "Current versions:"
ls "$RELEASES_DIR"
