#!/bin/bash
# DevStack plugin: Glance Go as the image service (devstack@g-go).
# Go only (default): lib/glance-go-only does the work inside DevStack's own
# glance steps; these phases only clean up. With GLANCE_GO_INSTALL_ORIGINAL:
# install (build), extra (bookkeeping, start in place of g-api).
# See docs/glance/devstack-plugin.md.

_XTRACE_GLANCE_GO=$(set +o | grep xtrace)
set -o xtrace

if is_service_enabled g-go; then
    if [[ "$GLANCE_GO_INSTALL_ORIGINAL" == "True" && "$1" == "stack" && "$2" == "install" ]]; then
        if ! is_service_enabled g-api; then
            die $LINENO "GLANCE_GO_INSTALL_ORIGINAL needs g-api enabled"
        fi
        echo_summary "Installing Glance Go"
        install_glance_go
    elif [[ "$GLANCE_GO_INSTALL_ORIGINAL" == "True" && "$1" == "stack" && "$2" == "extra" ]]; then
        echo_summary "Starting Glance Go in place of g-api"
        configure_glance_go
        start_glance_go
    fi
    if [[ "$1" == "unstack" ]]; then
        stop_process g-go
    fi
    if [[ "$1" == "clean" ]]; then
        sudo rm -rf "$(dirname "$GLANCE_GO_STORE_ROOT")"
    fi
fi

$_XTRACE_GLANCE_GO
