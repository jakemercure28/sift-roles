# Deploy (CI/CD)

The MacBook is the dev box. The ThinkPad runs everything. Code is never edited
on the ThinkPad; it only runs there.

## Topology

| Env     | Branch | Stack (compose project) | Host port            | Supabase project | Gemini key |
|---------|--------|-------------------------|----------------------|------------------|------------|
| staging | `dev`  | `jsa-staging`           | `127.0.0.1:3132`     | staging project  | dev key    |
| prod    | `main` | `jsa-prod`              | `127.0.0.1:3131`     | prod project     | prod key   |

Both stacks run on the one ThinkPad, served by a single self-hosted runner. Each
env has its own checkout under `/srv/jsa/<env>` carrying its own gitignored
`.env`, `data/`, and `.context/`. That is how the two stacks point at different
Supabase projects, Gemini keys, and ports without any change to git.

## Flow

1. Develop on the MacBook on a feature branch. The Stop hook auto-PRs and
   merges into `dev`.
2. **Push to `dev` deploys staging** (`.github/workflows/deploy.yml`).
3. When staging looks good, open a PR `dev -> main`. Branch protection requires
   the `guard` check (PR must come from `dev`) plus CI to pass.
4. **Merge to `main` deploys prod.**

Each deploy: a GitHub-hosted `gate` job runs `go build` + `go test`; only if
green does the self-hosted `deploy` job run `scripts/deploy.sh <env>`, which
fetches the branch into that env's checkout, rebuilds the stack, waits for every
service to report healthy, and rolls back to the previous commit if it doesn't.

Manual deploy: Actions tab -> Deploy -> Run workflow (pick the branch), or on the
box: `/srv/jsa/<env>/scripts/deploy.sh <staging|prod>`.

## GitHub setup (already applied)

- **Environments** `production` (deploys only from `main`) and `staging`
  (deploys only from `dev`).
- **Branch protection on `main`**: requires a PR, the `guard` check, and CI
  (`test`, `go-test`, `go-test-postgres`, `docker-build`); linear history; no
  force-push/delete; `enforce_admins=false` (you can override in a pinch).
- `dev` is intentionally unprotected so the auto-PR hook can auto-merge into it.

## Where the runner lives

**On the ThinkPad**, as a systemd service run by a dedicated `deploy` user (in
the `docker` group, not root). Pull-based: the runner dials *out* to GitHub over
HTTPS, so no inbound SSH port and nothing added to the Cloudflare Tunnel just to
deploy. Its labels must include `thinkpad` to match `runs-on: [self-hosted,
thinkpad]`.

---

# Provisioning checklist

> **STATUS (2026-06-15): provisioned and live.** Both stacks run on the ThinkPad
> (`jake@100.87.101.41`, ssh alias `thinkpad`). The runner runs as **`jake`**
> (already in the `docker` group), not a dedicated `deploy` user, so the
> `useradd deploy` steps below were skipped and `/srv/jsa` is owned by `jake`.
> Prod was cut over from the old `/home/jake/Projects/job-search-automation`
> (kept DOWN as a rollback fallback). Remaining: fill the staging Gemini key in
> `/srv/jsa/staging/.env`; set up the Cloudflare Tunnel for `siftroles.com`.
> The checklist below is kept as the from-scratch reference.

Do these in order. The GitHub side is done; the rest is on you (physical box +
Supabase).

## 1. Supabase

- [ ] **Prod** = your existing project. Confirm you have its pooled
      `DATABASE_URL` (Supavisor on `:6543`, not `:5432`), `SUPABASE_URL`, and
      `SUPABASE_ANON_KEY`.
- [ ] **Staging** = a NEW free Supabase project, fully isolated from prod data.
      Run the schema/migrations against it, then grab its own `DATABASE_URL`,
      `SUPABASE_URL`, and `SUPABASE_ANON_KEY`.
- [ ] In each Supabase project, enable Authentication -> Sign In / Providers ->
      Google and set that project's Google OAuth client ID/secret. In Google
      Cloud Console, add the project's callback URI:
      `https://<ref>.supabase.co/auth/v1/callback`.
- [ ] In each Supabase project's Authentication -> URL Configuration, set the
      environment origin as Site URL and Redirect URL (`https://siftroles.com` for
      prod, `https://staging.siftroles.com` for staging). Keep the legacy
      `anitajob.xyz` origins while those aliases are still live.
- [ ] RLS is still a launch TODO per project notes; tighten it on prod before
      public signup.

## 2. ThinkPad base

```bash
# Docker + compose + git installed, then:
sudo useradd -m -s /bin/bash deploy
sudo usermod -aG docker deploy
sudo mkdir -p /srv/jsa && sudo chown deploy:deploy /srv/jsa
sudo -iu deploy
```

## 3. Per-env checkouts + secrets

```bash
# as deploy
for env in prod staging; do
  git clone https://github.com/jakemercure28/job-search-automation.git /srv/jsa/$env
done
git -C /srv/jsa/prod    checkout main
git -C /srv/jsa/staging checkout dev

# Seed each env's gitignored files. These NEVER come from git.
#   /srv/jsa/prod/.env     -> prod DATABASE_URL/SUPABASE_*,
#                             prod GEMINI_API_KEY, DASHBOARD_PORT=3131,
#                             METRICS_PORT=9101, SCRAPER_METRICS_PORT=4041
#   /srv/jsa/staging/.env  -> staging DATABASE_URL/SUPABASE_*,
#                             dev GEMINI_API_KEY,  DASHBOARD_PORT=3132,
#                             METRICS_PORT=9102, SCRAPER_METRICS_PORT=4042
# Also copy data/ and .context/ into each (or run scripts/setup.sh per CLAUDE.md).
```

Set `DATABASE_TYPE=postgres` in both `.env` files (engine-agnostic image).

## 4. Self-hosted runner (one, serves both envs)

```bash
# as deploy, get the token from:
#   repo -> Settings -> Actions -> Runners -> New self-hosted runner
mkdir -p ~/actions-runner && cd ~/actions-runner
curl -o runner.tar.gz -L <runner-tarball-url>
tar xzf runner.tar.gz
./config.sh --url https://github.com/jakemercure28/job-search-automation \
            --token <registration-token> \
            --labels thinkpad \
            --name thinkpad \
            --work _work
sudo ./svc.sh install deploy   # run service as the deploy user
sudo ./svc.sh start
```

The runner's own `_work` dir is throwaway; deploys operate on `/srv/jsa/<env>`.

## 5. Cloudflare Tunnel

- [ ] Route the prod hostname -> `http://127.0.0.1:3131`.
- [ ] (Optional) a staging hostname -> `http://127.0.0.1:3132`, ideally behind
      Cloudflare Access so it isn't public.
- [ ] Route `grafana.<domain>` -> `http://127.0.0.1:3000` and gate it with
      Cloudflare Access. See `docs/observability.md` for the full observability
      stack (Prometheus + Grafana + Loki). The metrics ports above
      (`METRICS_PORT`, `SCRAPER_METRICS_PORT`) MUST differ between envs so both
      stacks can run on the one box.

## 6. First deploys

```bash
# Smoke each env by hand before relying on the workflow:
/srv/jsa/staging/scripts/deploy.sh staging
/srv/jsa/prod/scripts/deploy.sh prod
```

Then push to `dev` and confirm the Deploy workflow drives staging end to end.

## Notes

- **Don't `clean: true`** in checkout / don't `git clean -x` the env dirs; it
  would wipe the on-box `.env`, `data/`, `.context/`.
- The runner needs outbound HTTPS to GitHub only. No inbound ports.
- Staging is deliberately quiet (`docker-compose.staging.yml` disables
  scrape/score on boot and slows the cron) so deploys don't burn Gemini quota.
  Flip those back on in that file if you want staging to run the live pipeline.
