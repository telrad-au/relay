#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || { echo "Run this installer as root." >&2; exit 1; }
# The native installer owns all fixed targets, ACLs, migration, and rollback.
# This script only supplies the reviewed bundle's contents.
{
    printf '{"config":'
    cat relay.example.json
    printf ',"trust":'
    cat update-trust.json
    printf ',"installation":'
    cat installation-manifest.json
    printf '}\n'
} | ./telrad-relay install-native
