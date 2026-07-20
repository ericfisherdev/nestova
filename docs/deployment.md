# Deploying Nestova on the Raspberry Pi

The whole-appliance runbook: the Go server as a systemd service, HTTPS via
Tailscale (which is what makes the PWA installable), and the environment
knobs that make sessions behave correctly behind the proxy.

Related runbooks: [`kiosk.md`](kiosk.md) for the entryway touchscreen,
[`aws-backups.md`](aws-backups.md) for nightly backups,
[`aws-monitoring.md`](aws-monitoring.md) for the dead-man alarm,
[`pwa.md`](pwa.md) for what the HTTPS endpoint unlocks.

## The shape of it

```text
 phone / laptop ──HTTPS──▶ tailscale serve ──HTTP──▶ nestova on :8080
   (tailnet)               (TLS terminated,          (systemd service)
                            Let's Encrypt cert)

 kiosk on the LAN ──────────plain HTTP──────────────▶ same server, unchanged
```

Nestova itself never terminates TLS here. Tailscale does, and Nestova
recovers the original scheme from `X-Forwarded-Proto` — trusting that
header only from `TRUSTED_PROXIES` (loopback by default), so a same-host
proxy works without widening trust. Secure cookies and HSTS engage off
that derived scheme.

The kiosk keeps talking plain HTTP over the LAN and is unaffected by any
of this; its service worker simply never registers, which is by design
(see [`pwa.md`](pwa.md)).

## 0. Prerequisites on a fresh Pi

Everything below assumes these exist. On a brand-new or replacement Pi
they do not, and skipping this step fails in two confusing ways: the
`install` in step 1 aborts because the target directory is absent, and
systemd refuses to start the unit because its `User=` or
`EnvironmentFile=` is missing.

```sh
# Service account: no login shell, no home directory — it only runs the binary.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin nestova

# Directories: binary, config, and the writable state directory that the
# unit's ReadWritePaths= grants access to.
sudo install -d -o root    -g root    -m 0755 /opt/nestova/bin
sudo install -d -o root    -g nestova -m 0750 /etc/nestova
sudo install -d -o nestova -g nestova -m 0750 /var/lib/nestova
```

Postgres must be installed and running, since the unit both `Requires=`
and orders itself after `postgresql.service`:

```sh
sudo apt install -y postgresql
sudo systemctl enable --now postgresql
sudo -u postgres createuser --pwprompt nestova
sudo -u postgres createdb --owner=nestova nestova
```

Then create `/etc/nestova/server.env` with at least `DATABASE_URL`, plus
the knobs from step 3. It holds the database password, so lock it down:

```sh
sudo touch /etc/nestova/server.env
sudo chown root:nestova /etc/nestova/server.env
sudo chmod 0640 /etc/nestova/server.env
```

Root-owned and group-readable — the service user reads it, but cannot
rewrite its own configuration.

## 1. The server as a systemd service

Nestova must be running and restarting on its own before anything is put
in front of it.

```ini
# /etc/systemd/system/nestova-server.service
[Unit]
Description=Nestova household server
After=network-online.target postgresql.service
Wants=network-online.target
Requires=postgresql.service

[Service]
Type=simple
User=nestova
Group=nestova
WorkingDirectory=/opt/nestova
EnvironmentFile=/etc/nestova/server.env
ExecStart=/opt/nestova/bin/server
Restart=always
RestartSec=5
# PORT sets the listen port. Note the server binds ALL interfaces (the
# address is built as ":$PORT"); there is no bind-host setting. See
# "What the LAN can reach" below for what that means and how to narrow it.
Environment=PORT=8080

# Hardening: the server needs its data directory and nothing else.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/nestova
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
```

`/etc/nestova/server.env` holds `DATABASE_URL` and the knobs from step 3;
step 0 created it with the permissions it needs, since it contains the
database password.

```sh
sudo install -m 0755 -o root -g root server /opt/nestova/bin/server
sudo systemctl daemon-reload
sudo systemctl enable --now nestova-server.service
systemctl status nestova-server.service
curl -fsS http://localhost:8080/healthz && echo   # expect: ok
curl -fsS http://localhost:8080/readyz  && echo   # expect: ready
```

Check both, and treat `/readyz` as the one that matters here.
`/healthz` reports process liveness only — it never touches a backing
dependency, so it answers `ok` from a server whose database is
unreachable. `/readyz` runs a real database health check and returns 503
when Postgres is down. A deploy verified with `/healthz` alone can look
healthy while every actual request fails.

### What the LAN can reach

Be clear-eyed about this: **Nestova listens on every interface**, because
the listen address is constructed as `":$PORT"` with no host part and no
environment knob to change it. Tailscale HTTPS is *additive* — it gives
you a TLS entry point; it does not take the plain-HTTP one away.

So on a normal home LAN the app is reachable at
`http://<pi-lan-ip>:8080` without TLS. That is what the kiosk uses when
the display is a separate device, and it is the same trust boundary
[`kiosk.md`](kiosk.md) already states for the display itself: the home
network is inside the trusted perimeter.

If you want it narrower, do it at the network layer rather than in the
app — the server offers no way to bind a single address:

```sh
# Example: allow loopback and the LAN, drop anything else reaching :8080.
sudo ufw allow from 192.168.0.0/24 to any port 8080 proto tcp
sudo ufw deny 8080/tcp
```

A kiosk running on the Pi itself needs nothing beyond loopback, so that
deployment can restrict `:8080` to loopback entirely and still work.

## 2. HTTPS via Tailscale

A service worker — and therefore PWA install — requires a secure
context. `tailscale serve` provisions and auto-renews a real Let's
Encrypt certificate for the tailnet name, with no port forwarding and
nothing exposed to the public internet.

One-time, in the [Tailscale admin console](https://login.tailscale.com/admin/dns):

1. Enable **MagicDNS**.
2. Enable **HTTPS Certificates**.

Both are prerequisites — `tailscale serve` cannot obtain a certificate
without them.

Then on the Pi:

```sh
# Confirm the machine is on the tailnet and note its name.
tailscale status
# A stable machine name matters: it becomes the hostname in the URL, and
# renaming it invalidates the cert and any installed PWA's start_url.
sudo tailscale set --hostname=nestova

sudo tailscale serve --bg http://localhost:8080
tailscale serve status
```

The app is now at `https://nestova.<tailnet>.ts.net`, reachable by
tailnet devices from anywhere — on the home LAN or over mobile data.

Certificate renewal is automatic; there is no cron entry to add.

## 3. Environment knobs

In `/etc/nestova/server.env`:

```sh
TRUSTED_PROXIES=127.0.0.0/8,::1/128   # default; the proxy is same-host
SESSION_COOKIE_SECURE=true            # Secure cookies regardless of APP_ENV
HSTS_ENABLED=true                     # only once the hostname is stable
HSTS_MAX_AGE=4320h                    # 180 days (Go duration; "180d" is invalid)
PUBLIC_BASE_URL=https://nestova.<tailnet>.ts.net
```

`SESSION_COOKIE_SECURE=true` is what stops the session cookie being
rejected: `auto` ties Secure to `APP_ENV=prod`, so a non-prod appliance
serving real HTTPS would otherwise emit non-Secure cookies.

Enable `HSTS_ENABLED` **only after** the hostname is settled. HSTS is
sticky in browsers — a name you later abandon keeps forcing HTTPS on
clients that remember it.

`PUBLIC_BASE_URL` deserves care. Kiosk QR deep links do **not** need it:
`tailscale serve` forwards with `Host` already set to the MagicDNS name,
so the derived origin is already correct. But **WebAuthn passkeys
require it** — a Relying Party ID must be one fixed value pinned at
startup, so a deployment that leaves this empty simply does not offer
passkey registration (NES-136).

Set it, and then treat the host as permanent: **changing it orphans
every passkey ever registered**, because a browser will not present a
stored passkey to a Relying Party ID it was not registered under. That
is the same reason the machine name matters in step 2, and why renaming
is a recovery-time hazard rather than a cosmetic change. See
[`webauthn.md`](webauthn.md).

Restart after editing: `sudo systemctl restart nestova-server.service`.

## 4. Verification

Do all of it; several failure modes only appear on one path.

| Check | How |
|---|---|
| Server process is up | `curl -fsS http://localhost:8080/healthz` on the Pi |
| Database is reachable | `curl -fsS http://localhost:8080/readyz` on the Pi — 503 means Postgres is down |
| HTTPS + valid cert, on-LAN | Open `https://nestova.<tailnet>.ts.net` from a tailnet device on the home network |
| HTTPS + valid cert, off-LAN | Same URL from a phone on **mobile data** (tailnet, not LAN) |
| Sessions survive the proxy hop | Log in with a test account, navigate, and confirm you stay logged in |
| Service worker registers | DevTools > Application > Service Workers on the HTTPS URL |
| Install prompt offered | Chrome menu shows "Install app" / "Add to Home screen" |
| Kiosk unaffected | Load the kiosk over plain HTTP on the LAN — still works, no worker |

## 5. Reboot survivability

The one step most likely to be skipped, and the one that matters on an
appliance nobody logs into for months:

```sh
sudo reboot
```

After it comes back:

```sh
systemctl status nestova-server.service   # active (running)
tailscale serve status                    # the proxy config is still there
curl -fsS https://nestova.<tailnet>.ts.net/readyz && echo
```

`/readyz` rather than `/healthz` is the deliberate choice after a reboot:
this is exactly the moment Postgres and the app race each other to come
up, and the systemd ordering only sequences *starts*, not readiness. A
server that won its race against a database still finishing recovery
answers `/healthz` with `ok` while every real request fails.

`tailscale serve --bg` persists its configuration, and the systemd unit
is enabled — but verify rather than assume. A serve config that does not
survive a power cut turns a five-second outage into a silent one lasting
until someone notices the phone app stopped working.

## Replacing the Pi

Re-running this page end to end is the whole recovery procedure, plus:

1. Restore the database from the latest backup — see
   [`aws-backups.md`](aws-backups.md); its restore script and the
   migration status check are the verification.
2. Re-join Tailscale and **reuse the same machine name**, or the PWA's
   `start_url` and the installed app on every phone break.
3. Re-provision the kiosk device — [`kiosk.md`](kiosk.md).
