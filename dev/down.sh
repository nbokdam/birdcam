#!/usr/bin/env bash
# Stop the local OIDC test environment (secrets in dev/.secrets are kept;
# delete that directory for a completely fresh start).
docker compose -f "$(dirname "$0")/compose.yaml" down
