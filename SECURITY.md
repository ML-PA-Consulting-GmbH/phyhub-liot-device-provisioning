# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems.

Use GitHub's private vulnerability reporting instead: go to the **Security** tab
of this repository and choose **Report a vulnerability**. This creates a private
advisory visible only to you and the maintainers, and lets us coordinate a fix
and a disclosure date with you.

If you cannot use that channel, email **security@ml-pa.com** and include enough
detail to reproduce the issue. Please do not include customer data or device
credentials in the report.

## What to expect

We aim to acknowledge a report within three working days and to give you an
initial assessment, including whether we consider it in scope, within ten
working days. We will keep you updated while we work on a fix, and we will
credit you in the advisory unless you prefer otherwise.

## Scope

In scope: the code in this repository, including its build and release
workflows.

Out of scope: findings that require physical access to already-provisioned
hardware, vulnerabilities in third-party dependencies that are already public
with an upstream fix pending, and automated scanner output without a
demonstrated impact.

## Supported versions

Only the default branch receives security fixes. This repository publishes
tooling rather than a supported product and carries no service level agreement.
If you use it in production, pin a commit and watch this repository for
advisories.
