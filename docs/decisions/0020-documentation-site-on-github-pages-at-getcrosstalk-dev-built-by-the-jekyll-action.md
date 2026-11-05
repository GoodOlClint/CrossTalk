# ADR 0020 — Documentation site on GitHub Pages at getcrosstalk.dev built by the Jekyll action

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator (scope and interview 2026-11-05) + Claude (Opus 5.5)
- **Context source:** `docs/plans/release-0.1.4.md` section 7b and 8

## Context

CrossTalk's docs are Markdown in `docs/` read on GitHub. The operator wants a site at `getcrosstalk.dev` that the Homebrew formula's `homepage` can name. `docs/` mixes user docs (`setup.md`), the design and the ADRs with working notes (`plans/`, `spikes/`). The repo has no Ruby, Node or Python toolchain, and CI should not gain one for docs. GitHub Pages serves one site per repository, each with its own custom domain, so this uses only the CrossTalk repo's site.

## Decision

- Build `docs/` with `actions/jekyll-build-pages` (GitHub's own Jekyll in its own container; no toolchain installed in the workflow, no dependency added to the repo) and deploy with `actions/upload-pages-artifact` and `actions/deploy-pages` from `.github/workflows/pages.yml`.
- Publish a short `docs/index.md` home page, `setup.md`, `design.md` and `docs/decisions/`; `docs/_config.yml` excludes `plans/` and `spikes/`. Links from published pages into excluded folders are absolute GitHub URLs.
- The apex `getcrosstalk.dev` is the site; `www.getcrosstalk.dev` redirects to it. HTTPS is enforced (the `.dev` top-level domain is HSTS-preloaded, so browsers never use plain HTTP there).
- The custom domain is set in the repo's Pages settings, not a `CNAME` file: a Pages site deployed by a custom Actions workflow ignores `CNAME`.
- The build job runs on pull requests that touch `docs/` (build only, no deploy), so a Jekyll error fails the PR; the deploy job runs on push to `main` only.

## Rejected alternatives

- **Hugo or MkDocs.** Better navigation and search, but a toolchain to install and pin in CI and a theme to maintain (interview).
- **Hand-written HTML.** The site and the Markdown would drift.
- **Publishing all of `docs/`.** Plans and spikes are working notes; they stay readable on GitHub (interview).
- **Branch-based Pages (`gh-pages` or `/docs` on `main` without a workflow).** Builds outside the repo's CI, so a broken build shows only in the Pages settings, and pull requests are not checked.
- **`www` as the main host.** The formula homepage and the operator's request name the apex (interview).

## Consequences

- The operator buys the domain, sets DNS (four apex `A` and four `AAAA` records for GitHub Pages, `www` `CNAME` to `goodolclint.github.io`), verifies the domain for the account, sets Pages source to GitHub Actions, sets the custom domain and enables Enforce HTTPS (plan section 9).
- `actions/jekyll-build-pages` last released v1.0.13 in 2024; if it is retired, the fallback is a pinned Ruby setup with the `github-pages` gem, which is the toolchain this decision avoids.
- A docs change on `main` publishes within minutes; there is no versioned site per release.

## Trial record

- Rests on: ADR 0018 (formula `homepage`).
- Commits: none yet. Implementing commits carry `Provisional-ADR: 0020`.
- Pinning tests (planned): `TestSiteConfig`, `TestSiteLinks`, `TestWorkflowHygiene` (covers `pages.yml`).
- Not yet run live: the first deploy, the certificate, the `www` redirect.
