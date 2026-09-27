# Security checks

`lint` and `review` scan the **added lines** of every changed text file, source and configuration alike (YAML, JSON, `.env`, Dockerfiles, Terraform, shell scripts…), with regular expressions. Nothing is executed and no model is involved. Removed lines, deleted files, binary files and generated lock files (`*.lock`, `*.sum`, `package-lock.json`, `pnpm-lock.yaml`…) are not scanned.

These are review prompts, not verified vulnerabilities: a match may be a fixture, an example or a false positive, and a change that raises nothing may still add a secret in a format the patterns do not know, an encoded value, or a misconfiguration spelled differently.

| Kind | Severity | What matches |
| --- | --- | --- |
| `private_key` | critical | A PEM private key header (`RSA`, `DSA`, `EC`, `OPENSSH`, `PGP`, `ENCRYPTED` or plain `PRIVATE KEY`) or a PuTTY key file header. |
| `hardcoded_secret` | high | Credential shapes: AWS access key IDs and secret keys, GitHub and GitLab tokens, Slack tokens and webhook URLs, Stripe live keys, Google API keys, LLM provider keys (`sk-`, `sk-ant-`, `sk-proj-`), npm tokens, Twilio and SendGrid keys, Azure storage account keys, JSON Web Tokens. Also a secret-named key (`password`, `passwd`, `pwd`, `secret`, `api_key`, `access_key`, `auth_token`, `access_token`, `refresh_token`, `private_key`, `client_secret`, `credentials`, with prefixes and suffixes) assigned a quoted literal of at least 8 characters containing a digit or a symbol. |
| `credential_in_url` | high | `scheme://user:password@host`, or a secret-named query parameter (`api_key`, `apikey`, `access_token`, `auth_token`, `token`, `secret`, `client_secret`, `password`, `passwd`, `sig`, `signature`) with a value of at least 8 characters. |
| `tls_verification_disabled` | high | `InsecureSkipVerify: true`, `rejectUnauthorized: false`, `NODE_TLS_REJECT_UNAUTHORIZED=0`, `verify=False`, `ssl_verify: false`, `CURLOPT_SSL_VERIFYPEER` off, `ssl._create_unverified_context`, `check_hostname = False`, `CERT_NONE`, `danger_accept_invalid_certs(true)`, `curl -k`/`--insecure`, `wget --no-check-certificate`, `sslmode=disable`, `tls.VersionSSL30`/`TLS10`/`TLS11`, `http.sslVerify false`. |
| `excessive_permissions` | high | `chmod 777`/`666`/`a+w`/`o+w`, `0777`/`0666` passed to file-system APIs or as a `mode`, `privileged: true`, `--privileged`, `allowPrivilegeEscalation: true`, `runAsUser: 0`, `runAsNonRoot: false`, `USER root`, `NOPASSWD: ALL`, `hostPID`/`hostNetwork: true`, `ALL` or `SYS_ADMIN` capabilities, a mounted `/var/run/docker.sock`. |
| `protection_disabled` | high | `@csrf_exempt`, CSRF set to false or `csrf().disable()`, `WTF_CSRF_ENABLED = False`, wildcard CORS (`Access-Control-Allow-Origin: *`, `AllowAllOrigins: true`, `origin: "*"`, `allow_origins=["*"]`), `SECURE_SSL_REDIRECT`/`SESSION_COOKIE_SECURE = False`, `HttpOnly`/`secure` false, `autoescape=False`, `{{ … \| safe }}`, JWT `none` algorithm or `verify_signature: False`, `setenforce 0`, `SELINUX=disabled`, `ufw disable`, seccomp or AppArmor `unconfined`, workflow `permissions: write-all`, `0.0.0.0/0` ingress, `publicly_accessible = true`, public-read ACLs, `X-Frame-Options: ALLOWALL`, Helmet without a content security policy. |
| `debug_enabled` | medium | `DEBUG = True`, `debug: true`, `app.run(… debug=True)`, `FLASK_DEBUG=1`, `APP_DEBUG=true`, `DJANGO_DEBUG=True`, `app.debug = True`, `gin.SetMode(gin.DebugMode)`. |
| `hardcoded_email` | low | An e-mail address. |
| `hardcoded_ip` | low | An IPv4 address inside quotes, after `://` or after `@`. |

## Exclusions

- **Placeholders and indirection.** Credential values such as `changeme`, `example…`, `xxx`, `<your-token>`, `${VAR}`, `$VAR`, `{{ var }}`, `%s`, `test…`, `dummy…` are ignored, as are secret-named assignments on a line that reads the value from elsewhere (`getenv`, `process.env`, `os.environ`, `secrets.`, `vault`, `config.`, `${{ … }}`), values without a digit or a symbol other than `_`, `-` and `.` (a key name such as `password_hash`), and paths or URLs (`/run/secrets`, `https://vault…`).
- **E-mail and IP addresses** are not checked in tests, documentation (`.md`, `.rst`, `.txt`, `.adoc`, `.html`), licence, notice, authors and code-owner files, or `testdata` and `fixtures` directories. Documentation domains (`example.com`, `.test`, `.invalid`, `.local`…), GitHub no-reply addresses, loopback, `0.0.0.0`, broadcast and the documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`) are ignored.
- **Severity by location.** In test files every high or critical match is **medium**: fixtures often carry fake keys and test-only settings, but a real key there leaks all the same. In documentation (`.md`, `.rst`, `.adoc`) a misconfiguration pattern is **low**, since it is usually quoted; a credential there stays **medium**.

At most 3 signals of one kind are raised per file; the first one counts the matching added lines. Signals land on the matching added line and join the review ranges like every other signal; a high or critical one requests human review (exit 2 with `--ci`).

## What is written

A matched secret is **never copied** into the signal: the evidence names the pattern (for example "AWS access key ID") and says that the value is not copied. Misconfiguration and address signals quote the line, cut to 240 characters, after the report's usual redaction. The diff itself is still recorded in the report with the report's best-effort redaction (see [security boundaries](SECURITY.md)); rotate any real credential a change exposed, whatever the report shows.
