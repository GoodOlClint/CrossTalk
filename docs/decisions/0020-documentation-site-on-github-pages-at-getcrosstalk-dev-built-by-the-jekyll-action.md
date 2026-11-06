# ADR 0020 — Documentation site on GitHub Pages at getcrosstalk.dev built by the Jekyll action

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator (scope and interview 2026-11-05) + Claude (Opus 5.5)
- **Context source:** `docs/plans/release-0.1.4.md` section 7b and 8

## Context

CrossTalk's docs are Markdown in `docs/` read on GitHub. The operator wants a site at `getcrosstalk.dev` that the Homebrew formula's `homepage` can name. `docs/` mixes user docs (`setup.md`), the design and the ADRs with working notes (`plans/`, `spikes/`). The repo has no Ruby, Node or Python toolchain, and CI should not gain one for docs. GitHub Pages serves one site per repository, each with its own custom domain, so this uses only the CrossTalk repo's site.

## Decision

- Build `docs/` with `actions/jekyll-build-pages` (GitHub's own Jekyll in its own container; no toolchain installed in the workflow, no dependency added to the repo) and deploy with `actions/upload-pages-artifact` and `actions/deploy-pages` from `.github/workflows/pages.yml`.
- Publish every page in `docs/` except working notes: `docs/_config.yml` excludes `plans/` and `spikes/`. Today that is the `index.md` home page, `why.md`, `how-it-works.md`, `examples.md`, `faq.md`, `setup.md`, `contributing.md`, `design.md` and `docs/decisions/`. Links from published pages into excluded folders are absolute GitHub URLs.
- The apex `getcrosstalk.dev` is the site; `www.getcrosstalk.dev` redirects to it. HTTPS is enforced (the `.dev` top-level domain is HSTS-preloaded, so browsers never use plain HTTP there).
- The custom domain is set in the repo's Pages settings, not a `CNAME` file: a Pages site deployed by a custom Actions workflow ignores `CNAME`.
- The build job runs on pull requests that touch `docs/` (build only, no deploy), so a Jekyll error fails the PR; the deploy job runs on every other event: a push to `main` that touches `docs/` or `pages.yml`, or a manual `workflow_dispatch`. The workflow has no ref guard, so a dispatch deploys from whatever ref it names; only the `github-pages` environment's deployment-branch rule, a repo setting, can restrict that.

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
- Commits (no `Provisional-ADR` trailer was used): 59d693b P2 Pages workflow; 05537d1 separate concurrency group for pull requests; aabfe75 site overhaul (why, examples, how-it-works, FAQ, contributing).
- Pinning tests: `TestSiteConfig`, `TestSiteLinks`, `TestSiteLinksPages`, `TestSiteNamesEveryTool`, `TestWorkflowHygiene` (covers `pages.yml`).
- Run live (checked 2026-11-06): `https://getcrosstalk.dev` answers 200 over HTTPS from GitHub Pages, and `https://www.getcrosstalk.dev` answers 301 to the apex.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (published page list; deploy also runs on `workflow_dispatch` from any ref; Trial record was "none yet"). Whether to add a `main`-only guard to the deploy job is left to the operator. Status stays Provisional.
