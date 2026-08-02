#!/bin/sh
export VARROA_API_HOST="${VARROA_API_HOST:-varroa-varroa-bff.varroa-system.svc.cluster.local}"
exec /docker-entrypoint.sh "$@"
