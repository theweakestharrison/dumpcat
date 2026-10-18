# Security policy

DumpCat holds database credentials, cloud storage keys and backup encryption passphrases, and it can restore data into
production servers. Security reports are taken seriously.

## Supported versions

Security fixes are released for the latest version only. Update with **System → Updates**, `dumpcat update`, or by
installing the newest package.

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report it privately through GitHub's private vulnerability reporting: **Security → Report a vulnerability** on the
[DumpCat repository](https://github.com/theweakestharrison/DumpCat/security). If that is not available, contact the maintainer,
[@theweakestharrison](https://github.com/theweakestharrison), and ask for a private channel before sending details.

Please include:

- the DumpCat version (`dumpcat version`) and the operating system;
- the affected component (web UI, API, updater, installer, a backup or restore engine…);
- steps to reproduce, or a proof of concept;
- what an attacker can achieve, and what access they need first.

Please give the maintainer reasonable time to release a fix before you disclose the problem publicly.

## Hardening an installation

- Put DumpCat behind HTTPS and restrict who can reach it, by IP or VPN.
- Enable two-factor login for admins.
- Keep the backup encryption passphrase and `master.key` somewhere safe and separate from the backups.
- If you publish your own builds, sign your releases so installations refuse a tampered update.

Details are in the manual: [Security](docs/manual.md#11-security) and [Updating](docs/manual.md#36-updating).
