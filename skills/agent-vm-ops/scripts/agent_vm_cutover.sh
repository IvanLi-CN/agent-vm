#!/bin/sh
exec "$(dirname "$0")/../../../deploy/scripts/agent_vm_cutover.sh" "$@"
