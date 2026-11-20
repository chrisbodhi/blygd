# Deploying blygd on one host

One small always-on Linux VM runs everything: blygd, Postgres 18 and Caddy
for TLS. About $6 a month on DigitalOcean, AWS Lightsail, Hetzner, Vultr or
similar. These steps were run on Debian 13; other systemd distros need only
different package steps. The files are in [`deploy/`](../deploy).

## Pick the host

| Need | Why |
|---|---|
| 1 GiB RAM, 1 vCPU, 20 GiB+ disk | 512 MiB runs, but exports and `pg_dump` leave no headroom. Disk holds the database and a week of dumps. |
| An always-on VM | The poller and mention workers run in the background, so scale-to-zero platforms (Cloud Run, Lambda) do not fit. |
| Postgres you run yourself | blygd holds a long-lived `LISTEN/NOTIFY` connection for `/api/events`. A transaction-mode pooler (PgBouncer, Supabase's pooler) or a serverless Postgres that suspends when idle breaks it. |
| Postgres 18 | Distros lag; use the PGDG repo below. |
| Public IPv4 | IPv6 as well is free and worth enabling. |

Turn on the provider's weekly VM backup if it is a dollar or so. It saves
you rebuilding the OS; it is not a substitute for the dumps below.

## 1. DNS and the domain

Point `A` (and `AAAA`) records at the VM before starting Caddy, or its
certificate requests fail and count against Let's Encrypt's limits. If the
domain has a `CAA` record, it must allow `letsencrypt.org`.

Choose the host name with care. `BLYGD_PUBLIC_URL` is where origins mount
(`https://host/~{handle}/`), published items stay permanent (DESIGN.md
"Invariants"), and the URL is the origin's identity. To try things first, use
a throwaway name such as `<ip-with-dashes>.sslip.io`, then drop and recreate
the database before real use. A host chosen later changes only where new
users mount; see "Changing the public host".

## 2. First login and hardening

As root, once:

```sh
apt update && apt full-upgrade -y
apt install -y ufw unattended-upgrades
adduser <you> && usermod -aG sudo <you>
install -d -m 700 -o <you> -g <you> /home/<you>/.ssh
cp /root/.ssh/authorized_keys /home/<you>/.ssh/ && chown <you>:<you> /home/<you>/.ssh/authorized_keys

printf 'PermitRootLogin no\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/00-hardening.conf
sshd -t && systemctl restart ssh
ufw allow OpenSSH && ufw allow 80,443/tcp && ufw enable
```

- Keep the root session open until a second terminal logs in as `<you>`
  and `sudo -v` works (set a password with `passwd <you>` first).
- The file is `sshd_config.d`, not `ssh_config.d` (that is the client's).
- Name it `00-…`. sshd uses the first value it reads, and cloud images ship
  `50-cloud-init.conf` and a `PermitRootLogin yes` line in `sshd_config`.
- Check with `sshd -T | grep -Ei 'permitrootlogin|passwordauth'`, and that
  `ssh root@host` is refused from a new terminal.
- `ufw status` should list 22, 80 and 443 for both v4 and v6.

## 3. Swap and Postgres 18

```sh
sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab

sudo apt install -y postgresql-common
sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh
sudo apt install -y postgresql-18

sudo tee /etc/postgresql/18/main/conf.d/small.conf <<'EOF'
shared_buffers = 128MB
max_connections = 30
work_mem = 4MB
maintenance_work_mem = 64MB
effective_cache_size = 384MB
EOF
sudo systemctl restart postgresql
sudo -u postgres psql -c 'select version();'
```

Scale those numbers with RAM. Check the PGDG page for current instructions.

## 4. Caddy

From Caddy's own apt repo (see <https://caddyserver.com/docs/install>):

```sh
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy
```

## 5. blygd

Build on your own machine and copy the binary up (no Go toolchain on the VM):

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/blygd-linux ./cmd/blygd   # arm64 hosts: GOARCH=arm64
scp bin/blygd-linux <you>@<host>:/tmp/blygd
scp -r deploy <you>@<host>:/tmp/deploy
```

On the VM:

```sh
sudo useradd --system --home /var/lib/blygd --create-home --shell /usr/sbin/nologin blygd
sudo install -m755 /tmp/blygd /usr/local/bin/blygd
sudo install -m755 /tmp/deploy/backup.sh /usr/local/bin/blygd-backup
sudo install -d -m750 -o blygd -g blygd /var/backups/blygd

sudo install -d -m750 -o root -g blygd /etc/blygd
sudo install -m640 -g blygd /tmp/deploy/blygd.env.example /etc/blygd/blygd.env
sudo nano /etc/blygd/blygd.env              # set BLYGD_PUBLIC_URL

# The role and database share the OS user's name: peer auth over the unix
# socket, so there is no database password.
sudo -u postgres createuser blygd
sudo -u postgres createdb -O blygd blygd

sudo install -m644 /tmp/deploy/blygd.service /tmp/deploy/blygd-backup.service /tmp/deploy/blygd-backup.timer /etc/systemd/system/
sudo install -m644 /tmp/deploy/Caddyfile /etc/caddy/Caddyfile
sudo nano /etc/caddy/Caddyfile              # replace blyg.example
sudo mkdir -p /var/log/caddy && sudo chown caddy:caddy /var/log/caddy

sudo systemctl daemon-reload
sudo systemctl enable --now blygd blygd-backup.timer
sudo systemctl reload caddy
```

blygd listens on `127.0.0.1:8080` and only Caddy is exposed. That is what
makes `BLYGD_TRUST_PROXY=true` safe: anyone who could reach blygd directly
could forge the `X-Forwarded-For` address that rate limits are charged to.

## 6. Check it, then make a user

```sh
sudo journalctl -u blygd -n 30 --no-pager     # needs sudo, or an empty log is shown
curl -sI http://127.0.0.1:8080/healthz
curl -4 -sI https://<host>/healthz
curl -6 -sI https://<host>/healthz
sudo journalctl -u caddy -n 20 --no-pager     # certificate obtained?

sudo -u blygd sh -c 'set -a; . /etc/blygd/blygd.env; blygd user add <handle> "Title"'
```

That prints an owner token once; keep it in a password manager. The env file
is `root:blygd 0640`, so source it inside the `blygd` user's shell (an
`$(grep …)` outside `sudo` runs as you and cannot read it). Use the same
pattern for `blygd token mint`, `rebuild` and `export`. Then open
`https://<host>/studio/` and paste the token. A handle is 1–63 lowercase letters, digits or
hyphens, and the blyg is at `https://<host>/~<handle>/`.

Expect scanners within minutes (new certificates appear in public
Certificate Transparency logs). They get 404s; blygd serves only what is in
`surface_files`.

## 7. Backups

`blygd-backup.timer` runs `backup.sh` nightly (about 03:15 UTC): a
`pg_dump -Fc`, checked with `pg_restore --list`, kept for seven days in
`/var/backups/blygd` (mode 0600, owned by `blygd`).

Prove a restore once, into a scratch database:

```sh
sudo systemctl start blygd-backup && sudo journalctl -u blygd-backup -n 20 --no-pager
sudo -u postgres createdb scratch
sudo sh -c 'cat "$(ls -t /var/backups/blygd/blygd-*.dump | head -1)"' | sudo -u postgres pg_restore -d scratch --no-owner
sudo -u postgres psql scratch -c '\dt'
sudo -u postgres dropdb scratch
```

### Offsite copy

Dumps on the same disk as the database are not a backup. Any S3-compatible
store works; this uses Backblaze B2 with an **upload-only** key, so a
compromised VM cannot delete or read what it has already sent.

1. Create a private bucket. Add a lifecycle rule (hide after 30 days, delete
   a day later), since the VM's key cannot prune.
2. The web console's "Write Only" preset also grants `deleteFiles` and
   lifecycle control. Make the key with the CLI instead, using the master
   key once and regenerating it afterwards:
   ```sh
   b2 key create --bucket <bucket> blygdBackup listBuckets,writeFiles
   ```
3. Install a current rclone. Debian's packaged one is too old for B2's
   login (`not currently supported on API version number 1`); use the
   `.deb` from <https://downloads.rclone.org/> and `sudo apt install ./rclone-*.deb`.
4. `sudo install -m 600 -o root -g root /dev/null /etc/blygd/backup.env`, then:
   ```
   RCLONE_REMOTE=b2:<bucket>
   RCLONE_CONFIG_B2_TYPE=b2
   RCLONE_CONFIG_B2_ACCOUNT=<key id>
   RCLONE_CONFIG_B2_KEY=<application key>
   ```
   systemd reads it as root, so `600` is enough. A key without `listFiles`
   is why `backup.sh` passes `--no-check-dest`.
5. `sudo systemctl start blygd-backup` should log `uploaded to b2:…`; confirm
   the file in the console.

For restores, make a separate read-only key
(`listBuckets,listFiles,readFiles`) and keep it off the server.

## 8. Operating for other people

Once anyone else publishes here, a failure is no longer only yours to
notice, and you hold powers over their blygs that they should know about.

### Alerts

- **Uptime.** Point an external monitor at `https://<host>/healthz`, which
  answers 200 only while blygd can reach Postgres. Any provider that
  checks a URL every few minutes will do; it learns only the URL.
- **Backups.** Make a dead-man's-switch check (healthchecks.io and the
  like) expecting one ping a day, with a few hours' grace past 03:15 UTC,
  and put its URL in `/etc/blygd/backup.env`:
  ```
  BLYGD_BACKUP_PING_URL=https://<the check's ping URL>
  ```
  `backup.sh` fetches it last, only after the dump was written, listed,
  sent offsite and pruned. A night with no ping is the alert. A failed
  ping does not fail the backup. `scripts/deploy.sh` installs each
  release's `backup.sh` as `blygd-backup`, so the ping arrives with the
  first deploy that carries it; on a host set up by hand, install it as in
  step 5. Check it once: `sudo systemctl start blygd-backup`, then
  `sudo journalctl -u blygd-backup -n 5 --no-pager` should end with
  `pinged the monitor`.
- **Queues.** Once a minute blygd logs one `queues` line: mentions to
  verify and send, origins polled, imports waiting, and how many of each
  are more than 15 minutes late. Late means a worker is behind or stopped,
  and the line is then a warning:
  ```sh
  sudo journalctl -u blygd -p warning --since today --no-pager
  ```
  `send_abandoned_24h` counts mentions that gave up after their retries in
  the last day; a rise means a receiver, or the network, is refusing them.
  A user's imports are late only when nothing in their list has been
  claimed or finished for 15 minutes, so a big OPML import that the worker
  is working through, one entry a batch, does not warn.

### People

Run these as the `blygd` user, as in step 6:

```sh
sudo -u blygd sh -c 'set -a; . /etc/blygd/blygd.env; blygd <command>'
```

| To | Command |
|---|---|
| Add someone | `user add <handle> "Title"`. The handle is in their URL for good. Send the owner token through a one-time secret link, never plain chat. They paste it into `/studio/` once and keep it in a password manager. |
| Invite people | `invite --count 10 --expires 7d --note "launch"`. Each code makes one user, who picks their own handle: the command prints each code beside a link, `https://<host>/studio/#invite/<code>`, which opens the studio's form with the code filled in (the code rides in the fragment, so it never reaches a log). The studio shows them their permanent URL before they submit, and their owner token once after. Codes are shown once and stored hashed; a refused code costs the address as a wrong token does. To see what is spent: `psql -c 'SELECT note, expires_at, redeemed_at, u.handle FROM invites LEFT JOIN users u ON u.id = redeemed_by'`. To withdraw unused ones: `UPDATE invites SET expires_at = now() WHERE redeemed_at IS NULL`. |
| Replace a lost owner token | `token mint <handle> owner '*'`. Revoke the old one from the new token if it may have leaked (`DELETE /api/tokens/{id}`). |
| Suspend someone | `user suspend <handle>`. Every token they hold is revoked, their open event streams end within 25 s, and every mention their blyg had yet to send is abandoned, so their queued stubs and quotes stop reaching anyone. A send already in flight may still arrive, once, but is never retried. A publish that authenticated just before the revoke can still queue sends after it, so run the command again a minute later to catch them. What they published stays published; their subscriptions are still polled. Minting them a token lifts it; abandoned sends stay abandoned. |
| Revoke every token, nothing else | `token revoke-all <handle>`, after a leak, say. |
| Take an item down | Suspend them if needed, then `token mint <handle> takedown --expires 1d items:withdraw` and `POST /api/items/{id}/withdraw` with it. The item becomes an endcap (§9) and its unpinned versions stop being served. **Pinned versions are served forever** (§8, enforced in the schema), and nothing here removes one. |

Tell testers, before their first publish:

- Publishing is permanent: versions are never deleted, a withdrawal leaves
  an endcap, and a pin cannot be undone. Drafts and preview are the place
  to experiment.
- The operator can act as any user (the commands above), and can read
  everything in the database, drafts included.
- After an upgrade that widens the studio's token, sign out of the studio
  and back in once.
- What other testers can and cannot see of their reading: the section below,
  in words they can take as they are.

### What testers can see of each other

Tell testers this as it stands; it is accurate and complete.

**Private to you.** No other tester can list, open or change your
subscriptions, what you call them, what you have read, your imports, your
private hoppers, your drafts, your tokens, or how you moderate the mentions
you receive. Every route of the API is tested for exactly that: another
user's ids answer as if they did not exist.

**Public because you chose it.** Your published items, your blogroll if you
add subscriptions to it, and any hopper you make public. A public hopper
lists items from blygs you follow, so it shows that you follow them.

**What another tester can work out.** blygd keeps one shared copy of each
blyg it reads, fetched once for everyone who follows it, so that a blyg's
author is not polled once per reader. Because of that, when you subscribe
to a blyg you can tell whether someone here already follows it, or once
did: it arrives with its history already in place, its last poll is older
than your subscription, and a "poll now" can be refused because someone
else polled it in the last minute. You learn *that someone here reads it*,
never who, how many, or what they have read. It is the same thing you could
learn by asking the room "does anyone here read X?", and the room is small.

**What the operator can see.** Everything in the database, drafts and
subscriptions included, and they can act as any user (the commands above).
That is true of every hosted service; here it is said out loud.

DESIGN.md ("What one user can infer of another's reading") has the reasons,
and what closing it would cost.

### A room on one address

At a hackathon or a meetup everyone reaches blygd from the venue's one
public address, so every per-address budget is shared by the room:
origin pages, hopper pages and the studio's files (600 a minute, bursts of
300), and failed authentication (30, then 10 a minute). Preflights and
requests with no token cost nothing, so only a client sending a wrong or
revoked token spends the room's authentication budget; the studio signs
out on a refused token instead of retrying. With `BLYGD_TRUST_PROXY=true`
behind Caddy the address is the venue's, as it should be. The budgets are
constants in `internal/web/limits.go` (`BLYGD_RATE_LIMITS` only turns them
all on or off), so a bigger allowance for an event is a change and a
deploy: decide before the day, from `journalctl -u blygd | grep status=429`
during the testers' week. Never turn proxy trust off to spread the load: the
budgets would then be charged to Caddy, one address for everyone.

## Upgrading

### Before you ship

On your own machine, from a clean checkout of what you will deploy:

```sh
make test-all      # everything, against a local Postgres 18
make vuln          # known vulnerabilities on paths blygd calls
```

When the change follows the spec or the reference client moving, also run
`make parity` and `make interop` against a current checkout of
`blygger-studio` (`scripts/interop/README.md`), and read the spec text you
are changing to, not these docs: CLAUDE.md says the spec wins.

A new file in `internal/db/migrations/` changes the database, and that does
not roll back with the binary (migrations are append-only and there are no
down migrations). That is what the preflight below is for.

### Ship it

From your machine, with a committed tree (the script builds the commit,
not your working files):

```sh
scripts/deploy.sh --check <you>@<host>     # preflight only: installs nothing
scripts/deploy.sh <you>@<host>             # ship HEAD; or name a commit after the host
```

`scripts/deploy.sh` builds the commit from a clean clone for the host's
architecture, copies it up with that commit's
[`deploy/upgrade.sh`](../deploy/upgrade.sh), and runs that under `sudo`
(it asks for your password). On the server:

1. **Preflight.** A fresh backup (offsite too), restored into a scratch
   database, `blygd_preflight`. The new binary migrates it and rebuilds
   every origin, so the release meets real data before the live database
   does. `--check` stops here. Run it before any release that migrates.
2. **Migrate.** blygd stops, the new binary migrates the live database, and
   only then is it installed (the old one is kept as `blygd.prev`) and
   started. Caddy holds requests while it is down (`lb_try_duration`), so
   visitors wait a few seconds rather than see a 502; open event streams
   drop and reconnect.
3. **Check.** `/healthz` answers, and the running process is the binary
   that was built (its SHA-256). If not, and nothing migrated, the old
   binary is put back and started.
4. **Rebuild** every origin, so existing pages pick up rendering changes.
   It rewrites only the re-renderable files: pins and item documents never
   change (DESIGN.md "Invariants").

A real deploy then adds its row to [DEPLOYS.md](DEPLOYS.md): the commit,
toolchain, checksum and migration. Write its notes and commit it. Nothing
else records what is live, though the server can say: `blygd version`
prints the commit, Go and x/net, and the "listening" log line carries the
commit. A commit ending `-dirty` was built from a tree with changes
(untracked files included), so it is not that commit; the script refuses
to ship one.

When `deploy/Caddyfile` changes, copy the change into
`/etc/caddy/Caddyfile` by hand (it carries your host name), then
`sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy`.
Caddy reads the file only at start and on reload, so confirm the running
config took it: `curl -s localhost:2019/config/` on the server shows what
is loaded (for `lb_try_duration`, a `load_balancing` block).

#### By hand

What the script does, for when it cannot run:

```sh
# on your machine (arm64 hosts: GOARCH=arm64)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/blygd-linux ./cmd/blygd
scp bin/blygd-linux <you>@<host>:/tmp/blygd

# on the server
sudo -u blygd sh -c 'set -a; . /etc/blygd/blygd.env; blygd migrate status'
sudo systemctl start blygd-backup                      # a fresh dump, offsite too
sudo cp /usr/local/bin/blygd /usr/local/bin/blygd.prev
sudo install -m755 /tmp/blygd /usr/local/bin/blygd
sudo systemctl restart blygd                           # migrates on start
sudo journalctl -u blygd -n 30 --no-pager              # "migrated" from..to, then "listening" with its commit
curl -4 -sI https://<host>/healthz
sudo -u blygd sh -c 'set -a; . /etc/blygd/blygd.env; blygd rebuild <handle>'
blygd version                                          # commit, Go and x/net, for DEPLOYS.md
```

With `BLYGD_MIGRATE_ON_START=false` in `blygd.env`, a new binary refuses to
start on an old schema until you run `blygd migrate`. The script migrates
explicitly either way.

### Rolling back

- **No migration in the release:** put the old binary back and restart.
  ```sh
  sudo install -m755 /usr/local/bin/blygd.prev /usr/local/bin/blygd && sudo systemctl restart blygd
  ```
- **The release migrated:** the old binary refuses to run on a database
  newer than itself. Either ship a fixed binary forward, or restore the
  pre-upgrade dump into a fresh database and point blygd at it, which
  loses whatever was written since the dump. Prefer the first; test a
  migration against a copy of a real dump before it ships.

### A new spec version

Keep the changes in their own commits and ship them together only after
`make interop` passes. Two things to settle by reading the new text, not by
assumption: what the `blyg` version string in manifests and items
(`blyg.ProtocolVersion`, written at render time) becomes, and how documents
already published, which are permanent, are to be read by a studio on the
new version. Anything the spec turns into a deliberate difference from the
code goes in DESIGN.md.

## Changing the public host

A new `BLYGD_PUBLIC_URL` moves nobody. An origin's URL is its identity
(§12.2) and what it published is permanent, so every origin keeps the host
it was made on, and the old host is served for as long as the instance
runs. What changes: new users (`user add`, an invite redeemed) mount under
the new host, invite links are printed with it, and only it is answered
in-process (DESIGN.md "Same-instance fetches"); origins on the old host are
fetched over the network, through Caddy, like anyone else's. One handle is
one user and one origin, so a handle taken on the old host is taken on the
new one too.

Until the first user exists on the new host, putting the old settings back
undoes it. After that, both hosts are permanent.

1. DNS: `A` and `AAAA` for the new name, as in step 1. Keep the old name's
   records for good.
2. A fresh backup: `sudo systemctl start blygd-backup`.
3. Caddy: the new host gets the site block, and the old one the commented
   block at the end of [`deploy/Caddyfile`](../deploy/Caddyfile), which
   sends `/studio/` to the new host. The studio's invite form shows the
   new URL under the host it was loaded from, so it must only ever be
   loaded from the public host; invite links already sent keep working,
   because the `#invite/` fragment survives the redirect. Validate, reload,
   and check the journal for the new certificate.
4. Set `BLYGD_PUBLIC_URL` in `/etc/blygd/blygd.env` and
   `sudo systemctl restart blygd`. The `listening` line shows the new
   `public`. Nothing needs rebuilding: an origin's pages are written from
   its own `base_url`.
5. Check `/healthz` on both hosts, an old origin's `blyg.json` on the old
   host, the old `/studio/`'s 308, and `/api/me` on both. Then publish from
   a new-host origin something that quotes an old-host origin, and the
   reverse, and see each mention verify: those are the fetches that now go
   over the network.
6. Point the uptime check at the new host's `/healthz`, keeping the old.
7. Tell users their blygs have not moved, but the studio has: a browser
   keeps the token per host, so they paste it once at the new `/studio/`.
   The API answers on both hosts, so their other clients keep working.

Add a DEPLOYS.md row for it, though no binary changed.

## Keeping it patched

Go programs carry their own copy of the standard library, TLS included, so
the OS package manager does not patch them. Three things on this host are Go:

- **blygd.** Run `make vuln` before each release and now and then between
  them, since an advisory against the toolchain or `x/net` needs a rebuilt
  binary, not a server update. Bump `toolchain` and the module, rebuild with
  the new toolchain (`go version -m bin/blygd-linux` shows what it embeds),
  and ship it as above.
- **Caddy**, from its own apt repo, and **Postgres 18**, from PGDG.
  `unattended-upgrades` covers only Debian's security repos by default, so
  run `sudo apt update && sudo apt upgrade` monthly, and after a Go
  advisory look for a new `caddy` (`apt list --upgradable | grep caddy`).
- **rclone**, installed from a `.deb`, which apt does not track. Check
  <https://downloads.rclone.org/> now and then and reinstall the same way.

Reboot after kernel updates (`/var/run/reboot-required` exists when one is
waiting). blygd and the backup timer start on boot.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `journalctl -u blygd` shows "No entries" | Run it with `sudo`, or join the `systemd-journal` group. |
| `BLYGD_DATABASE_URL is required` from a CLI command | The env file was read outside the `blygd` user; see step 6. |
| `ssh root@host` still works | The drop-in is in the wrong directory or sorts after `50-cloud-init.conf`; the daemon was not restarted. |
| Backup exits 127 | `rclone` is missing or not on the service's path. |
| Backup: `API version number 1` | rclone is too old. |
| Caddy cannot get a certificate | DNS has not propagated, ports 80/443 are closed (`ufw status`), or a `CAA` record forbids the CA. |
| Postgres `conf.d` file ignored | `postgresql.conf` lacks an `include_dir 'conf.d'` line. |

Watch `free -m` and `df -h` for a week. Sustained memory over about 400 MB
or climbing swap means the next size up.
