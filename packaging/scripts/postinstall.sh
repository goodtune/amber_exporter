#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    # The unit ships disabled: the exporter cannot start without an API key.
    # Put the key in /etc/amber_exporter/environment, then:
    #   systemctl enable --now amber_exporter.service
    #
    # On an upgrade, start the new version if the service is running.
    systemctl try-restart amber_exporter.service 2>/dev/null || true
fi
