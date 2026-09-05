#!/bin/bash
set -euo pipefail
# Scaffold a single-tenant env + volumes.
# Usage: TENANT_ID=acme API_PORT=8101 VNC_PORT=5901 NOVNC_PORT=6081 PROVIDER=chatgpt ./scripts/new-tenant.sh

TENANT_ID="${TENANT_ID:?set TENANT_ID}"
API_PORT="${API_PORT:-8101}"
VNC_PORT="${VNC_PORT:-5901}"
NOVNC_PORT="${NOVNC_PORT:-6081}"
PROVIDER="${PROVIDER:-chatgpt}"

rand() { openssl rand -hex 16 2>/dev/null || python3 -c "import secrets;print(secrets.token_hex(16))"; }

TENANT_DIR="tenants/${TENANT_ID}"
ENV_FILE="tenants/${TENANT_ID}.env"
mkdir -p "${TENANT_DIR}/browser_data" "${TENANT_DIR}/logs"

if [ -f "${ENV_FILE}" ]; then
  echo "exists: ${ENV_FILE} (not overwriting tokens)"
else
  API_TOKEN="$(rand)"
  VNC_PASSWORD="$(rand | cut -c1-16)"
  cat > "${ENV_FILE}" <<EOF
# Tenant ${TENANT_ID} — DO NOT COMMIT
TENANT_ID=${TENANT_ID}
PROVIDER=${PROVIDER}
API_TOKEN=${API_TOKEN}
VNC_PASSWORD=${VNC_PASSWORD}
API_PORT=8000
BROWSER_DATA_DIR=/app/browser_data
LOG_DIR=/app/logs
LOG_LEVEL=info
VERBOSE=false
RATE_LIMIT_SECONDS=2
EOF
  chmod 600 "${ENV_FILE}"
  echo "wrote ${ENV_FILE}"
fi

cat <<EOF
tenant=${TENANT_ID} ready
  env:    ${ENV_FILE}
  data:   ${TENANT_DIR}/browser_data
  up:     TENANT_ID=${TENANT_ID} API_PORT=${API_PORT} VNC_PORT=${VNC_PORT} NOVNC_PORT=${NOVNC_PORT} docker compose -f docker-compose.tenant.yml up -d --build
  login:  http://localhost:${NOVNC_PORT}/vnc.html
  api:    curl -H "Authorization: Bearer \$API_TOKEN" http://localhost:${API_PORT}/v1/models
  token:  \$(grep API_TOKEN ${ENV_FILE} | cut -d= -f2)
EOF
