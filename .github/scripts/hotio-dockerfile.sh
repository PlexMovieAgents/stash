#!/bin/bash
# Prints a Dockerfile that builds stash the way docker/build/x86_64/Dockerfile
# does, but links it statically and puts it into the hotio/stash image in place
# of the release build that image comes with. The hotio image is Ubuntu, while
# the build stage is Alpine, hence the static binary.
#
#   hotio-dockerfile.sh <hotio image>
#
# Fails if the upstream Dockerfile no longer has the lines this replaces.
set -euo pipefail

upstream=docker/build/x86_64/Dockerfile
build='RUN make flags-release flags-pie stash'
final='# Final Runnable Image'

for line in "$build" "$final"; do
  if ! grep -qxF "$line" "$upstream"; then
    echo "$upstream: no line '$line'" >&2
    exit 1
  fi
done

sed -e "s|^$build\$|RUN make flags-release flags-static-pie stash|" -e "/^$final\$/,\$d" "$upstream"
cat <<EOF
# The hotio image, with this build of stash
FROM $1
COPY --from=backend /stash/stash /app/stash
EOF
