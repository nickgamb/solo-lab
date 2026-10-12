# The lab on AWS

The whole lab on one EC2 instance, the same kind cluster as on a laptop,
served under public names with Let's Encrypt certificates:
`https://<name>.<party>.<ip-dashed>.sslip.io`, or under a domain of your own.
Outside IdPs, authorization servers and MCP clients reach it like any
public site.

## What you need

- The [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html),
  signed in with a profile:

  ```bash
  aws configure set region us-east-2 --profile solo-lab
  aws login --profile solo-lab
  ```

- An account on a paid plan, with 16 vCPUs of on-demand standard instances
  (Service Quotas, code `L-1216C47A`). The free plan only launches
  free-tier instance types.
- The `.env` you use on a laptop (editions, licence keys, IdP settings), with
  an OpenAI or Anthropic key: the instance has no Ollama, so it uses
  whichever of the two `.env` has a key for.

## Bring it up

```bash
make aws-up
```

`scripts/aws.sh up` creates whatever is missing, each tagged `Name=solo-lab`:

| Resource | |
| --- | --- |
| Key pair | ed25519, private key in `.lab/aws/solo-lab.pem` |
| Security group | 22 from the address you run this from, 80 from anywhere (Let's Encrypt), 443 from `AWS_HTTPS_CIDRS` |
| Elastic IP | the lab's names: `*.<party>.<ip-dashed>.sslip.io` |
| Instance | m7i.4xlarge (16 vCPU, 64 GiB), Ubuntu 24.04, 200 GB gp3, IMDSv2 only |

On first boot the instance installs Docker, kind, kubectl, helm, yq and the
rest of the requirements, and raises the inotify limits. Then `aws-up`
copies the lab over: the working tree, `.lab/keys` and `.lab/secrets.env`
(so the keys and secrets other parties already know stay the same), and
`.env` with these appended:

```bash
LAB_TLD=<ip-dashed>.sslip.io      # or AWS_LAB_TLD
LAB_LISTEN_ADDRESS=0.0.0.0        # the edge listens on the instance's address
LAB_CERT_ISSUER=letsencrypt       # each party's hostnames, by HTTP-01
LLM_PROVIDER=openai               # in place of ollama, when .env has the key
```

It then runs `make machine-setup` there (the lab's names resolve to the
instance itself) and `make up`, with the output followed here. A dropped
connection doesn't stop the run: `./scripts/aws.sh logs up` picks it up again.

When it finishes, `make aws-status` prints the names. Start at
`https://observatory.ops.<ip-dashed>.sslip.io` and
`https://kagent.sterling.<ip-dashed>.sslip.io`.

## Day to day

| Command | |
| --- | --- |
| `make aws-run-verify` | any make target on the instance (`aws-run-tour`, `aws-run-bob-verify`, ...) |
| `make aws-sync` | copy the lab over again after a change here; then `make aws-run-platform` |
| `make aws-ssh` | a shell there, in the lab's directory |
| `make aws-stop` | stop the instance: no compute charge, the disk and address stay |
| `make aws-start` | start it again; the cluster comes back with Docker |
| `make aws-status` | the instance, its address and the lab's names |
| `make aws-down` | terminate the instance and its disk; the address, security group and key pair stay, so the names stay |
| `make aws-down ALL=1` | remove everything |

## Settings

In `.env`:

| Setting | Default | |
| --- | --- | --- |
| `AWS_PROFILE` | `solo-lab` | the CLI profile |
| `AWS_REGION` | `us-east-2` | |
| `AWS_NAME` | `solo-lab` | the `Name` tag on everything, and the key pair's name |
| `AWS_INSTANCE_TYPE` | `m7i.4xlarge` | 16 GB is the lab's floor; 64 GiB leaves room for builds |
| `AWS_DISK_GB` | `200` | |
| `AWS_HTTPS_CIDRS` | `0.0.0.0/0` | who may reach port 443, comma-separated |
| `AWS_LAB_TLD` | `<ip-dashed>.sslip.io` | your own domain, with a wildcard A record for `*.<domain>` to the Elastic IP |
| `AWS_ENV` | | more `.env` lines for the instance only (or a file: `.lab/aws/env`) |
| `LAB_ACME_EMAIL` | | Let's Encrypt's contact for expiry notices |
| `LAB_ACME_SERVER` | Let's Encrypt production | the staging directory for rehearsals |
| `MCP_UPSTREAM_CIMD` | `false` | `true` in `AWS_ENV`: the gateway names itself to outside MCP servers' authorization servers by its own client ID metadata document (`make upstream-mcp`, [IDENTITY-FLOWS.md](IDENTITY-FLOWS.md)) |

Let's Encrypt certificates are kept on the instance in `.lab/edge-certs/` and
reused by later builds of the same names until 30 days before they expire.

## Outside IdPs and authorization servers

The names change with the address, so register the instance's URLs where the
laptop's are registered today:

| Where | URL |
| --- | --- |
| Each upstream IdP's client for S&V's broker (redirect URI) | `https://idp.sterling.<tld>/realms/sterling-vance/broker/<alias>/endpoint` |
| Ledgerline's AS: S&V's broker as a trusted issuer | `https://idp.sterling.<tld>/realms/sterling-vance` |
| Ledgerline's AS: S&V's broker keys | `https://idp.sterling.<tld>/realms/sterling-vance/protocol/openid-connect/certs` |

User emails don't change: S&V's users keep `@sterling.lab`
(`SV_MAIL_DOMAIN`), so their accounts at outside IdPs still match.
