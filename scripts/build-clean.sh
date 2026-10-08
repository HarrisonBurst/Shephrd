#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  cli) commands=(shephrd) ;;
  all) commands=(shephrd shephrd-terminal-herdr shephrd-terminal-cmux shephrd-notification-macos shephrd-repository-scanner shephrd-github-observer) ;;
  *) echo "usage: $0 [cli|all]" >&2; exit 2 ;;
esac

root="$(git rev-parse --show-toplevel)"
commit="$(git -C "$root" rev-parse HEAD)"
build="$(mktemp -d "${TMPDIR:-/tmp}/shephrd-clean.XXXXXX")"
repin_config=""
trap 'rm -rf "$build"; if [ -n "$repin_config" ]; then rm -f "$repin_config"; fi' EXIT

git clone --shared --quiet "$root" "$build/src"
if [ "$(git -C "$build/src" rev-parse HEAD)" != "$commit" ]; then
  echo "build-clean: clone is not at $commit" >&2
  exit 1
fi
if [ -n "$(git -C "$build/src" status --porcelain)" ]; then
  echo "build-clean: clone is not a clean checkout" >&2
  exit 1
fi

for command in "${commands[@]}"; do
  (cd "$build/src" && go build -trimpath -o "$build/$command" "./cmd/$command")
  stamp="$(go version -m "$build/$command")"
  if printf '%s' "$stamp" | grep -q '+dirty'; then
    echo "build-clean: $command carries a dirty stamp" >&2
    printf '%s\n' "$stamp" | grep '+dirty' >&2
    exit 1
  fi
  if ! printf '%s' "$stamp" | grep -q "$commit"; then
    echo "build-clean: $command does not carry revision $commit" >&2
    exit 1
  fi
done

extension_pin=""
config="$HOME/.config/shephrd/config.toml"
if [ -f "$config" ]; then
  extension_pin="$(sed -n '/^\[terminal_extensions\.herdr\]/,/^\[/s/^sha256 = "\(.*\)"/\1/p' "$config" | head -1)"
fi

if printf '%s\n' "${commands[@]}" | grep -qx shephrd-terminal-herdr && [ -n "$extension_pin" ]; then
  built_pin="$(shasum -a 256 "$build/shephrd-terminal-herdr" | awk '{print $1}')"
  if [ "$built_pin" != "$extension_pin" ]; then
    if [ "${SHEPHRD_BUILD_CLEAN_REPIN:-}" != 1 ]; then
      cat >&2 <<EOF
build-clean: the rebuilt Herdr terminal extension does not match the configured pin, so nothing was installed.
  built  $built_pin
  pinned $extension_pin
Set the built hash in the pinned terminal_extensions.herdr configuration, then re-run this target to install.
EOF
      exit 1
    fi
    repin_config="$(mktemp "$config.XXXXXX")"
    sed "/^\[terminal_extensions\.herdr\]/,/^\[/s/^sha256 = \"$extension_pin\"/sha256 = \"$built_pin\"/" "$config" > "$repin_config"
  fi
fi

mkdir -p "$root/bin"
for command in "${commands[@]}"; do
  cp "$build/$command" "$root/bin/$command.tmp"
  chmod 755 "$root/bin/$command.tmp"
  mv -f "$root/bin/$command.tmp" "$root/bin/$command"
  if [ "$command" = shephrd-terminal-herdr ] && [ -n "$repin_config" ]; then
    mv -f "$repin_config" "$config"
  fi
done

echo "build-clean: installed ${commands[*]} from clean commit $commit"
if [ -n "$repin_config" ]; then
  cat <<EOF
build-clean: repinned $config to $built_pin
Set the flake terminal_extensions.herdr sha256 to $built_pin and run darwin-rebuild switch so the installed extension, live pin and flake pin agree.
EOF
fi
