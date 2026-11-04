#!/bin/sh
# Starts the mock with the built-in server and runs the contract test against it.
cd "$(dirname "$0")/.." || exit 1
STATE_FILE=/tmp/erp-test-state.json php -S 127.0.0.1:8081 index.php >/tmp/erp-test.log 2>&1 &
server=$!
sleep 1
php tests/run.php http://127.0.0.1:8081
status=$?
kill "$server"
exit "$status"
