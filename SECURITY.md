# Security policy

_This is meerkat's vulnerability disclosure policy. meerkat is open-source software published by
Primitive Engineering: the `mk` binary, its release archives, its container image and its Homebrew
formula. Operators run it themselves; there is no hosted meerkat service. This file is the single
point of contact for security matters about all of it._

Whether meerkat is a product within the EU Cyber Resilience Act is still being decided
(MK-A-9, under #68). The default assumption is that it is. Until that decision is made, this
policy already follows the CRA's disclosure practice.

## Report a vulnerability

- **Email:** `security@primitive-engineering.se`. A person reads it; it is not an automated intake.
- **Private advisory:** you can also report through GitHub. On this repository, open Security →
  Advisories → "Report a vulnerability". The report stays private until we publish an advisory.
- **Encryption:** meerkat has no OpenPGP key yet. Send a short description in clear by email, or
  put the details in a private advisory, which only the maintainers can read.
- **Please include:**
  - the affected version (`mk version`), image digest, or commit;
  - steps to reproduce, or a proof of concept;
  - the impact as you assess it.
- **Do not include** the content of someone else's knowledge base, memories or logs, even if you
  reached it. Tell us that you reached it and what kind of data it was.

We do not run a bug bounty. We credit reporters in the advisory unless they prefer otherwise.

## What to expect

| Stage | Commitment |
| --- | --- |
| Acknowledgement | within **3 business days** of receipt |
| Triage and severity (CVSS v3.1 or v4.0) | within **10 business days**; we tell you what we concluded and why |
| Fix | actively exploited: mitigated without undue delay. Other high or critical: target **30 days**. Medium or low: target **90 days** |
| Disclosure | coordinated with you. By default we publish when the fixed release ships, and at the latest **90 days** after triage unless we agree otherwise |

A fix ships as a patch release. A GitHub Security Advisory on this repository and the release notes
name the affected versions, the fixed version and any workaround. CVE IDs come from the company's
CNA once it is live. Until then, they come from GitHub's CNA, through the advisory.

## Safe harbour

We will not take legal action against good-faith research that follows this policy. For software
that other people deploy, good faith means:

- test only against meerkat instances you run yourself, or have the operator's permission to test;
- stop as soon as you reach data that is not yours, and report it;
- no data destruction, no service degradation, and no volumetric testing against anyone's deployment;
- no social engineering and no physical attacks.

## Scope

- **In scope:**
  - this repository;
  - the release archives and their signatures;
  - the container image `ghcr.io/zegit-zoo/meerkat`;
  - the formula in [zegit-zoo/homebrew-tap](https://github.com/zegit-zoo/homebrew-tap).
- **Documented trade-offs:** [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) lists the risks meerkat
  accepts, each with its reason. Examples: neither server terminates TLS, and `mk http serve` has one
  shared key. Report one of these if you can show it is worse than the document says.
- **Out of scope:**
  - the content an operator chooses to mount;
  - an operator's own deployment choices, such as exposing a server without a TLS proxy;
  - third-party identity providers, object stores and forges themselves;
  - volumetric denial of service, social engineering and physical attacks.
- **Dependencies:** if the flaw is in a dependency (a Go module or the base image), tell us anyway.
  We report it upstream and track the fixed version.

## Supported versions

meerkat is pre-1.0. Security fixes ship for the **latest release** only, as a patch on the newest
minor version. Older versions get no fix unless a release note says otherwise. To upgrade, use
`mk update`, `brew upgrade meerkat`, or a newer image tag. The declared support period will be set
when the CRA scope is decided (MK-A-9).

## Actively exploited vulnerabilities

For an actively exploited vulnerability or a severe incident, we follow the CRA Art. 14 practice: an
early warning within 24 hours, a notification within 72 hours, and a final report, made to the
coordinator CSIRT and ENISA through the single reporting platform. Reporters are told when their
report triggers a notification.

## Security engineering

[docs/SECURITY.md](docs/SECURITY.md) covers the scanners and signatures every release goes through.
[docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) covers the trust boundaries, the assets and the
controls.
