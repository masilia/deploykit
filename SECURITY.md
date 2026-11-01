# Security policy

deploykit runs commands on servers over SSH and handles credentials, so reports about its security are welcome.

**Please do not open a public issue for a vulnerability.** Use GitHub's private reporting: the repository's *Security*
tab, then *Report a vulnerability*. Include the version (`deploykit version`), what you did and what you expected.

Out of scope: weaknesses of the servers, CloudPanel or Docker setups it deploys to.

Design rules worth knowing when you review it: secrets travel through the environment or stdin and never in a command
argument (one documented exception, `docs/adr/0006-clpctl-password-argv.md`); other targets' inventory is never copied to
a server; error messages do not echo values that could be secrets.
