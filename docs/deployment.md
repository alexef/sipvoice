# Deployment

## Why it needs real UDP

SIP signalling here runs over UDP, and call audio (RTP) is always UDP: 50
packets per second in each direction, sent between ports negotiated in the SDP.
That rules out:

- HTTP/SOCKS proxies and userspace VPN tunnels that only carry TCP
- most sandboxed/serverless platforms and some cloud dev boxes (DNS answers
  may even be fake addresses)
- networks that block outbound UDP 5060 or high UDP ports

Check with `sipvoice probe`. If it times out with "no response (UDP blocked?)",
run sipvoice somewhere else.

## Running on a LAN host behind NAT

A small always-on machine on a home or office network (Raspberry Pi, NAS, mini
PC) works well:

- No port forwarding is needed for outbound calls. sipvoice adds `rport`, learns
  its public address from the provider's `Via received/rport`, uses it in
  Contact/SDP, and starts sending RTP right away so the NAT mapping opens. It
  also locks onto the address that media actually comes from (symmetric RTP).
- Most providers anchor media and handle NAT for you. If audio is one-way, check
  your router's SIP ALG (often best disabled) and UDP session timeouts.
- sipvoice never registers, so it can run next to an IP phone or router that
  is registered on the same account without taking its incoming calls.

## Deploying over SSH

```sh
# build for the target (static binary, no CGO)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o sipvoice ./cmd/sipvoice

scp sipvoice config.example.yaml host:~/sipvoice/
ssh host 'chmod 700 ~/sipvoice && mv ~/sipvoice/config.example.yaml ~/sipvoice/config.yaml'
# edit ~/sipvoice/config.yaml; put secrets in an env file only you can read:
ssh host 'umask 077; cat > ~/sipvoice/.env' <<'ENV'
SIP_USERNAME=...
SIP_PASSWORD=...
XAI_API_KEY=...
ENV

ssh host 'cd ~/sipvoice && set -a && . ./.env && set +a && ./sipvoice --config config.yaml probe'
```

Start the daemon detached, with all file descriptors redirected so the SSH
session doesn't hang:

```sh
ssh -n -f host 'cd ~/sipvoice && set -a && . ./.env && set +a && \
  setsid nohup ./sipvoice --config config.yaml daemon >>daemon.log 2>&1 </dev/null &'
```

Stop it with `kill -TERM <pid>`, which hangs up any active call first. Avoid
`pkill -f sipvoice` over SSH: the pattern also matches your SSH shell's own
command line.

For a permanent setup use a systemd unit:

```ini
[Unit]
Description=sipvoice
After=network-online.target

[Service]
EnvironmentFile=%h/sipvoice/.env
ExecStart=%h/sipvoice/sipvoice --config %h/sipvoice/config.yaml daemon
Restart=on-failure
KillSignal=SIGTERM

[Install]
WantedBy=default.target
```

The HTTP API is unauthenticated: keep `listen` on `127.0.0.1` and reach it
over SSH (`ssh host ./sipvoice/sipvoice call ...` or an SSH port forward).
