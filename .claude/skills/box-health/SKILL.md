---
name: box-health
description: Investigate and monitor the health of the production droplet that serves eurypos.com and blyg.newschematic.org (blygd, Postgres 18, Caddy) using sysstat/sar, the DigitalOcean agent, systemd and blygd's own logs. Use when asked how the box or server is doing, about CPU, memory, disk, swap, load or network over time, why something got slow or went down, after a deploy or a reboot, or to check up on it routinely.
---

# Box health

One droplet runs everything: `blygd`, `postgresql@18-main` and `caddy`,
plus DigitalOcean's `do-agent` and `droplet-agent`. `docs/DEPLOY.md` is
how it was built and `docs/DEPLOYS.md` is what is running on it. This
skill is for looking, not changing anything.

## Connecting

```sh
ssh -o BatchMode=yes christopher@blyg.newschematic.org '<command>'
```

- Use `blyg.newschematic.org`. `eurypos.com` points at the same box, but
  its host key is not in `known_hosts`, so BatchMode refuses it.
- **Run nothing that changes the box.** Restarts, edits, `apt` and
  anything under `sudo` are the user's call.
- **sudo asks for a password**, so it fails over BatchMode. Everything
  below works without it unless marked *(sudo)*. For those, give the user
  the command to run themselves:
  `! ssh -t christopher@blyg.newschematic.org 'sudo …'`.
- The login is in the `systemd-journal` group, so `journalctl` reads every
  unit's log without sudo.
- Put several checks in one ssh call (quote it, separate with `;`) rather
  than one round trip per command.

## What normal looks like

Debian 13, **1 vCPU, 1 GB RAM, 1 GB swap, 25 GB disk** (a shared `s-1vcpu-1gb`
in sfo2). The clock and `sar` are in UTC. Baseline on 2026-11-26:

| | Normal | Worth a look | Trouble |
|---|---|---|---|
| Memory `available` (`free -h`) | ~550–600 MiB | < 250 MiB | < 100 MiB, or OOM kills |
| Swap used | tens of MiB, flat | climbing day on day | `sar -W` showing steady swap-in |
| Load (`sar -q`, 1 CPU) | well under 1 | > 1 for an hour | > 2 sustained |
| CPU `%steal` (`sar -u`) | ~0 | > 5 | > 15 (noisy neighbour: the box is starved, not busy) |
| CPU `%iowait` | ~0 | > 10 | > 30 |
| Disk `/` | ~14% | > 70% | > 85% |
| Memory pressure (`/proc/pressure/memory`, avg300) | ~0 | > 5 | > 20 |
| blygd memory (`systemctl show blygd -p MemoryCurrent`) | 15–45 MB | > 150 MB | climbing without limit |
| `queues` line | every `*_late=0`, `poll_failing=0` | any `_late` > 0 | `_late` growing, or no `queues` lines at all |

On a 1 GB box Postgres and the page cache take most of the "used" memory.
That is fine. Judge by `available` and by swap activity, not `free`.

## Triage, in this order

Run the first block in one call, then dig into whatever looks wrong:

```sh
uptime; free -h; df -h /; cat /proc/pressure/{cpu,memory,io}
systemctl list-units --failed --no-pager --plain
systemctl is-active blygd postgresql@18-main caddy do-agent
systemctl show blygd -p ActiveEnterTimestamp -p NRestarts -p MemoryCurrent -p CPUUsageNSec
journalctl -u blygd --since -5min -o cat --no-pager | grep -m1 msg=queues
journalctl -k --since -7d --no-pager | grep -iE 'oom|killed process|out of memory'
journalctl --list-boots --no-pager | tail -3     # `last` is not installed
```

And from your own machine, the check an outside monitor makes:

```sh
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' https://eurypos.com/healthz
```

`/healthz` answers 200 only while blygd can reach Postgres.

## Over time: sar

sysstat takes a sample every 10 minutes (`sysstat-collect.timer`) into
`/var/log/sysstat/saDD`, where DD is the day of the month. It keeps
`HISTORY` days (`grep HISTORY /etc/sysstat/sysstat`): Debian's default is
7, and `docs/DEPLOY.md` raises it to 28. Collection started
2026-11-26 at 18:48 UTC, so nothing before that exists. A `LINUX RESTART`
row marks a boot, or sadc starting fresh: the first one is the install,
not a reboot, so check `journalctl --list-boots` before calling one a
reboot. sar reports rates between two samples, so a row appears only
after the second sample following a restart.

```sh
sar -u                          # CPU: %user %system %iowait %steal %idle
sar -q                          # load averages, run queue, blocked tasks
sar -r --human                  # memory: kbavail is what matters
sar -S --human                  # swap used
sar -W                          # pswpin/s pswpout/s: swapping now, not just swap held
sar -B                          # paging, major faults
sar -d -p                       # I/O per device: %util, await
sar -n DEV                      # network per interface (eth0)
sar -n EDEV                     # network errors and drops
sar -r -f /var/log/sysstat/sa24 # another day
sar -u -s 14:00:00 -e 16:00:00  # one window
sar -A -s 14:00:00 -e 14:30:00  # everything, around one incident
```

To chart a trend, get machine-readable rows from `sadf` and pass them to a
chart tool or a quick script rather than eyeballing a table:

```sh
sadf -d /var/log/sysstat/sa26 -- -r     # memory as semicolon-separated rows
sadf -d /var/log/sysstat/sa26 -- -u     # CPU
```

For more than a week, the user can raise `HISTORY` *(sudo)*. Above 28,
files are named `saYYYYMMDD`, so `ls /var/log/sysstat` before naming one.

## Over time: DigitalOcean graphs

`do-agent` reports to the droplet's **Graphs** tab in the DigitalOcean
control panel: CPU, load, memory, disk usage, disk I/O and bandwidth.
Point the user there for history from before 2026-11-26 or older than
sar's week. Alert policies (memory above some percent, disk above 80%)
are set there too, and email the account owner. Nothing to run on the
box.

## Per service

```sh
systemd-cgtop -b -n 2 -d 2 --order=memory | grep -E 'blygd|postgres|caddy|agent' | tail -5
systemctl status blygd caddy postgresql@18-main --no-pager -n 0
```

cgtop prints two samples, and CPU is only filled in the second, so keep
the last five lines. Postgres is the largest at about 110 MB, then Caddy
at about 37 MB, then blygd. When a unit stops, the journal records what it used:
`journalctl -u blygd -o cat | grep Consumed`, which gives lines like
"Consumed 3min 11s CPU time, 43.2M memory peak".

## blygd's own signals

blygd logs logfmt to the journal. `-o cat` strips the journal prefix.

```sh
# Queues: one line a minute, a warning when something is >15 min late (DEPLOY.md §8)
journalctl -u blygd -p warning --since today -o cat --no-pager
journalctl -u blygd --since -1h -o cat --no-pager | grep msg=queues | tail -3

# Requests: status counts, then server errors, then the slowest
journalctl -u blygd --since -1d -o cat --no-pager | grep msg=request \
  | grep -o 'status=[0-9]*' | sort | uniq -c | sort -rn
journalctl -u blygd --since -1d -o cat --no-pager | grep msg=request | grep -E 'status=5[0-9][0-9]'
journalctl -u blygd --since -1d -o cat --no-pager | grep msg=request | grep -v path=/api/events \
  | awk '{for(i=1;i<=NF;i++) if($i~/^ms=/){split($i,a,"=");print a[2],$0}}' | sort -rn | head

# Rate limiting (DEPLOY.md "A room on one address")
journalctl -u blygd --since -1d -o cat --no-pager | grep -c status=429

# Starts, with the commit each one runs
journalctl -u blygd --since -7d -o cat --no-pager | grep -E 'msg=listening|msg=migrated'
```

`/api/events` is a long-lived event stream, logged when it closes with
its whole lifetime as `ms`, so it is left out of the slowest list.
Requests for `/wp-admin/…` and the like that get a 404 are scanners. They
are noise unless their volume shows in CPU.

The `listening` line's commit should match the newest row of
`docs/DEPLOYS.md`. Compare `sha256sum /usr/local/bin/blygd` with that
row's SHA-256 too.

## Postgres, Caddy, backups

```sh
journalctl -u postgresql@18-main --since -1d -o cat --no-pager | grep -vE 'checkpoint (starting|complete)' | tail -20
journalctl -u caddy --since -1d -o cat --no-pager | grep -E '"level":"(warn|error)"' | tail -10
systemctl list-timers blygd-backup.timer --no-pager
journalctl -u blygd-backup --since -2d -o cat --no-pager | grep -iE 'upload|ping|error|fail'
```

Each night should log `uploaded to b2:…`. If `BLYGD_BACKUP_PING_URL` is
set in `/etc/blygd/backup.env`, it should also log `pinged the monitor`.
As of 2026-11-26 it logs no ping, so that dead-man's switch is not
armed.

Caddy logs `aborting with incomplete response` when a client hangs up
mid-response, such as a closed event stream. A few is normal.

*(sudo)* sizes:

```sh
sudo -u postgres psql -c '\l+'                       # database sizes
sudo du -sh /var/lib/postgresql /var/backups/blygd   # data dir, local dumps
```

Without sudo, `journalctl --disk-usage` and `df -h /` still show the big
picture. The journal holds about 125 MB.

## Reporting back

Lead with a verdict: healthy, or the one thing that is wrong. Then a few
lines of numbers set against the "normal" table, each with its time
window. Say what you could not check because it needed sudo, and give
the user the command. For a trend over days, chart the `sadf` rows
instead of pasting sar tables. Suggest a change (raise `HISTORY`, add swap,
resize the droplet, a DigitalOcean alert) only with the numbers behind it,
and leave making it to the user.
