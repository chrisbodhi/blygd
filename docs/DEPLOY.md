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
(`https://host/{handle}/`), published items stay permanent (DESIGN.md
"Invariants"), and the URL is the origin's identity. To try things first, use
a throwaway name such as `<ip-with-dashes>.sslip.io`, then drop and recreate
the database before real use.

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
`https://<host>/studio/` and paste the token. A handle cannot contain a dot.

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

## Upgrading

```sh
# rebuild, scp to /tmp/blygd, then
sudo install -m755 /tmp/blygd /usr/local/bin/blygd && sudo systemctl restart blygd
```

blygd migrates on start, so expect a few seconds of downtime. Take a dump
first (`sudo systemctl start blygd-backup`). Set `BLYGD_MIGRATE_ON_START=false`
and run `blygd migrate` by hand if you want to separate the two.

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
