# Security posture

What this image does and does not promise, with a command to check each claim
rather than a badge. Everything here is about the **container**; the notes at
the bottom say which parts are the host's job instead.

## Verify it yourself

```bash
VCS_REF=$(git rev-parse --short HEAD) docker compose build

# Unprivileged, no capabilities, nothing writable
docker compose run --rm --entrypoint /bin/sh mqttview -c 'id; grep CapEff /proc/self/status'
docker compose run --rm --entrypoint /bin/sh mqttview -c 'touch /x'   # Read-only file system
docker compose run --rm --entrypoint /bin/sh mqttview -c 'command -v apk || echo gone'

# What is inside, and what a scanner makes of it
docker run --rm --entrypoint cat mqttview:latest \
  /usr/share/mqttview/sbom.cdx.json > sbom.json
trivy image --vex security/mqttview.openvex.json mqttview:latest
grype sbom:sbom.json --vex security/mqttview.openvex.json
```

A scan that reports nothing is only good news if it could see something. Check
that Trivy names the OS and a package count before believing a clean result:

```
INFO  Detected OS  family="alpine" version="3.21.7"
INFO  [alpine] Detecting vulnerabilities...  pkg_num=17
```

The package manager is removed from the image but `/lib/apk/db` is deliberately
kept, because that database is what those seventeen packages are read from.

## CIS Docker Benchmark

The benchmark covers the host, the daemon and the container. Only the last part
is decided by this repository; the rest is your machine, and no image can do it
for you.

| CIS section | Control | Where it is done | Check |
|---|---|---|---|
| 4.1 | Run as a non-root user | `Dockerfile` (`USER 10001`) | `id` inside the container |
| 4.2 | Use trusted base images | Every base pinned by SHA256 digest | `grep '@sha256:' Dockerfile` |
| 4.3 | No unnecessary packages | Alpine, package manager removed | `command -v apk` |
| 4.6 | Add a HEALTHCHECK | `Dockerfile`, runs the binary itself | `docker ps` shows healthy |
| 4.7 | Do not use update instructions alone | `apk upgrade` is in the same layer as the install | `Dockerfile` |
| 4.9 | Use COPY, not ADD | `Dockerfile` | `grep ADD Dockerfile` |
| 4.10 | No secrets in the image | `.env`, `*.db`, `config/` excluded | `.dockerignore` |
| 5.3 | Drop Linux capabilities | `cap_drop: [ALL]` | `grep CapEff /proc/self/status` → `0000000000000000` |
| 5.4 | No privileged containers | never set | `docker inspect --format '{{.HostConfig.Privileged}}'` |
| 5.7 | Do not map privileged ports | 8114, bound to loopback by default | `compose.yaml` |
| 5.9 | Do not share the host network | never set | `docker inspect --format '{{.HostConfig.NetworkMode}}'` |
| 5.10-5.11 | Memory and CPU limits | `mem_limit: 512m`, `cpus: 1.5` | `docker inspect --format '{{.HostConfig.Memory}}'` |
| 5.12 | Read-only root filesystem | `read_only: true` | `touch /x` fails |
| 5.25 | Restrict privilege escalation | `no-new-privileges:true` | `docker inspect --format '{{.HostConfig.SecurityOpt}}'` |
| 5.28 | Limit PIDs | `pids_limit: 256` | `docker inspect --format '{{.HostConfig.PidsLimit}}'` |
| 5.31 | Do not mount the Docker socket | never mounted | `compose.yaml` |

Also done, outside the benchmark: setuid and setgid bits are stripped from every
file in the image, `/tmp` is a `noexec,nosuid,nodev` tmpfs capped at 16 MiB, and
logs rotate at 10 MiB × 3 so a chatty broker cannot fill the host disk.

## Findings that cannot be fixed

`mqttview.openvex.json` records vulnerabilities that a scanner reports, that
have no fix available, and that we have analysed and found do not apply here.
Scanners read it directly:

```bash
trivy image --vex security/mqttview.openvex.json mqttview:latest
grype sbom:sbom.json --vex security/mqttview.openvex.json
```

The file is currently empty, which is the honest state: nothing has needed an
exception yet. A statement is added only with a written justification naming
why the vulnerable code path is not reachable in this image — not to quiet a
scanner. `ignore-unfixed` in CI covers the ordinary case of a finding with no
patch yet; VEX is for the ones where we are claiming the finding is wrong *for
this image*, which is a stronger claim and needs the reasoning to go with it.

## What is deliberately not claimed

* **No FIPS, STIG or CIS certification.** The table above says which controls
  the container satisfies. Nobody has audited it.
* **No patching service level.** The image is rebuilt weekly and on every push
  to `main`; that is a schedule, not a promise about response time.
* **Nothing about the host.** Kernel version, daemon configuration, user
  namespaces, seccomp beyond Docker's default, AppArmor or SELinux — all of
  that is yours. A container is not a security boundary on a host that has
  already been taken.
* **Nothing about your brokers.** mqttview holds broker credentials and TLS
  private keys and decrypts them to connect. An instance is as sensitive as the
  brokers it can reach; `SECURITY.md` covers what that means.

## The host's job

* Keep the Docker daemon and the kernel patched.
* Put a TLS-terminating reverse proxy in front, or configure TLS in mqttview.
  The compose file binds to loopback by default so that this is a decision
  rather than an accident.
* Back up the `/data` volume. It holds the encryption key; without it the
  stored broker credentials are unrecoverable.
* Restrict who can reach the port at all. Authentication is the application's
  job, but the network is yours.
