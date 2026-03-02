#!/usr/bin/env bash

# Repo-local Go module settings for corporate/work laptops.
# Usage:
#   source scripts/use-local-go-proxy.sh
#
# We use environment variables (not `go env -w`) so nothing changes globally.
export GOPROXY="https://proxy.golang.org,direct"
export GOSUMDB="sum.golang.org"

echo "GOPROXY=${GOPROXY}"
echo "GOSUMDB=${GOSUMDB}"

