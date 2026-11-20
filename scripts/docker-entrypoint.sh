#!/bin/sh
set -eu

CONFIG_PATH="${GIT_BACKUP_CONFIG:-/etc/git-backup/config.yaml}"
CRON_SCHEDULE="${CRON_SCHEDULE:-0 */4 * * *}"
WEEKLY_REPORT_CRON="${WEEKLY_REPORT_CRON:-}"
RUN_ON_START="${RUN_ON_START:-true}"

if [ "$#" -gt 0 ] && [ "$1" != "daemon" ]; then
  if [ "$1" = "serve" ] && [ "${RUN_ON_START:-false}" = "true" ]; then
    echo "Running initial git-backup sync before starting serve..."
    set +e
    /usr/local/bin/git-backup sync --config "${CONFIG_PATH}"
    rc=$?
    set -e
    if [ "$rc" -eq 1 ]; then
      echo "WARNING: initial sync exited with fatal code 1; starting server anyway." >&2
    fi
  fi
  exec /usr/local/bin/git-backup "$@"
fi

if [ ! -f "${CONFIG_PATH}" ]; then
  echo "ERROR: config file not found at ${CONFIG_PATH}" >&2
  echo "Mount your config.yaml to ${CONFIG_PATH} or set GIT_BACKUP_CONFIG." >&2
  exit 1
fi

if [ "${RUN_ON_START}" = "true" ]; then
  echo "Running initial git-backup sync..."
  set +e
  /usr/local/bin/git-backup sync --config "${CONFIG_PATH}"
  rc=$?
  set -e
  if [ "$rc" -eq 1 ]; then
    echo "WARNING: initial sync exited with fatal code 1; scheduler will continue." >&2
  fi
fi

CRON_FILE="/etc/crontabs/root"
printf '%s /usr/local/bin/git-backup sync --config "%s"\n' "${CRON_SCHEDULE}" "${CONFIG_PATH}" > "${CRON_FILE}"
if [ -n "${WEEKLY_REPORT_CRON}" ]; then
  printf '%s /usr/local/bin/git-backup report --weekly --config "%s"\n' "${WEEKLY_REPORT_CRON}" "${CONFIG_PATH}" >> "${CRON_FILE}"
fi

echo "Starting crond with schedule: ${CRON_SCHEDULE}"
exec crond -f -l 6
