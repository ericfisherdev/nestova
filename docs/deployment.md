# Deploying Nestova on the Raspberry Pi

The whole-appliance runbook: the Go server as a systemd service, HTTPS via
Tailscale (which is what makes the PWA installable), and the environment
knobs that make sessions behave correctly behind the proxy.

Related runbooks: [`kiosk.md`](kiosk.md) for the entryway touchscreen,
[`aws-backups.md`](aws-backups.md) for nightly backups,
[`aws-monitoring.md`](aws-monitoring.md) for the dead-man alarm,
[`pwa.md`](pwa.md) for what the HTTPS endpoint unlocks.

## The shape of it

```
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

`/etc/nestova/server.env` holds `DATABASE_URL` and the knobs from step 3
— `chmod 600`, owned by the service user, since it contains the database
password.

```sh
sudo install -m 0755 -o root -g root server /opt/nestova/bin/server
sudo systemctl daemon-reload
sudo systemctl enable --now nestova-server.service
systemctl status nestova-server.service
curl -fsS http://localhost:8080/healthz && echo   # expect: ok
```

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
| Server is up | `curl -fsS http://localhost:8080/healthz` on the Pi |
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
curl -fsS https://nestova.<tailnet>.ts.net/healthz && echo
```

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
