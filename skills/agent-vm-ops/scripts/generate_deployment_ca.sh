#!/bin/sh
exec "$(dirname "$0")/../../../deploy/scripts/generate_deployment_ca.sh" "$@"
