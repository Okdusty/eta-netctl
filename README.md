# eta-netctl

Minimal Android runtime bundle. Existing module names are preserved.

Files:

- `eta-net` — network session manager
- `eta-netctl` — service control command
- `eta-net-magisk-service.sh` — Magisk boot supervisor
- `eta-tun-direct.yml` — direct TUN fallback profile
- `bin/eta-ciadpi` — transparent TCP helper for Android ARM64
- `bin/eta-nfqws` — NFQUEUE helper for Android ARM64
- `bin/eta-dns-direct` — local DNS helper for Android ARM64
- `etadns` — source and tests for `eta-dns-direct`

No private keys, captures, device logs, server configuration, or generated build trees are included.
