# Deploying Sift for friends (Ubuntu ThinkPad + Cloudflare Tunnel)

This is the runbook for running the hosted, multi-tenant build on a home machine
and handing a signup link to friends. It assumes the app already runs locally via
`docker compose up -d --build` and that you have a hosted Supabase project and a
paid Gemini key.

The model: the dashboard stays bound to loopback (`127.0.0.1:3131`), and a
Cloudflare Tunnel running on the same machine reaches it locally and publishes it
on a public hostname over HTTPS. Nothing on your LAN or the public internet talks
to the container directly, so you do not expose a port or run a reverse proxy.

## 1. Environment (`.env`)

Set these on the ThinkPad. See `.env.example` for the full list and comments.

```
DATABASE_TYPE=postgres
DATABASE_URL=postgres://...           # pooled Supabase DSN (port 6543)
SUPABASE_URL=https://<ref>.supabase.co
SUPABASE_ANON_KEY=...

# Host AI for everyone, on your paid key (every tenant scores on this one key):
GEMINI_API_KEY=<your-paid-gemini-key>
GEMINI_HOST_DAILY_LIMIT=<your-plan-daily-ceiling>
GEMINI_HOST_PER_TENANT_DAILY_LIMIT=400        # per-tenant fairness cap (set above ~315/day on a paid key)
GEMINI_HOST_INACTIVE_DAILY_LIMIT=30           # daily budget for idle tenants (keep-warm); 0 disables

# Per-tenant request throttle on expensive endpoints (optional, on by default):
RATE_LIMIT_PER_MINUTE=20                       # 0 disables; self-host never throttled
RATE_LIMIT_BURST=5

# Postgres RLS defense-in-depth (optional, default off — see section 5):
# RLS_ENFORCE=true
# Self-hosted (co-located) Postgres — keeps the dashboard read path local, no pooler egress:
# RLS_DATABASE_URL=postgresql://app_rls:<pw>@supabase-db-prod:5432/postgres?sslmode=disable
# Hosted Supabase pooler (role name is app_rls.<project-ref>):
# RLS_DATABASE_URL=postgres://app_rls.<project-ref>:<pw>@<pooler-host>:6543/postgres?sslmode=require
```

Signup is open: anyone who completes Google OAuth gets a tenant. That is fine for
a quiet, link-only friends launch and keeps the door open for a wider public
launch later without any allowlist to maintain.

`GEMINI_HOST_PER_TENANT_DAILY_LIMIT` is the cost guardrail. With batch scoring it
lets each friend score a useful number of jobs per day without any single account
exhausting the shared key. It is the one setting that actually bounds your spend,
so keep it set.

Set `GEMINI_HOST_DAILY_LIMIT` to the ceiling your key really has, not the one you
wish it had. On the Gemini free tier that is 500 requests per model per day. Set
it higher and the app keeps dispatching into 429s, burning five 30 second retries
per job to rediscover the same limit, and no scoring gets done.

`GEMINI_HOST_INACTIVE_DAILY_LIMIT` is the companion fairness knob. Most signups
stop opening their dashboard after a week, but their queues keep filling, so
without a daily budget the idle majority spends the shared key on jobs nobody
reads. Set it near `SCORING_INACTIVE_TOP_N` so an inactive tenant costs about one
keep-warm pass a day and the rest of the key stays with people actually using
the app.

## 2. Domain + Cloudflare Tunnel

1. Add `siftroles.com` to a Cloudflare account and switch the domain to Cloudflare's
   nameservers at your registrar. The tunnel DNS route below only resolves once
   Cloudflare is authoritative for the domain. The domain must live in the **same
   Cloudflare account** that owns the tunnel, or `tunnel route dns` writes records
   into the wrong zone.
2. Install `cloudflared` on the ThinkPad and authenticate it against the target
   zone: `cloudflared tunnel login` (pick `siftroles.com` in the browser). If a
   `~/.cloudflared/cert.pem` already exists for a different zone, move it aside
   first, since login refuses to overwrite it.
3. Create a tunnel and route the hostnames to the local services:
   ```
   cloudflared tunnel create sift
   cloudflared tunnel route dns sift siftroles.com
   cloudflared tunnel route dns sift app.siftroles.com
   cloudflared tunnel route dns sift staging.siftroles.com
   cloudflared tunnel route dns sift grafana.siftroles.com
   ```
4. Point the tunnel ingress at the loopback services. In the tunnel config:
   ```yaml
   ingress:
     - hostname: siftroles.com
       service: http://localhost:3131
     - hostname: app.siftroles.com
       service: http://localhost:3131
     - hostname: staging.siftroles.com
       service: http://localhost:3132
     # Observability (optional): Grafana behind Cloudflare Access. See
     # docs/observability.md for the full Prometheus + Grafana + Loki stack.
     - hostname: grafana.siftroles.com
       service: http://localhost:3000
     - service: http_status:404
   ```
   The legacy `anitajob.xyz` hostnames can stay in the ingress and DNS as live
   aliases during the transition; nothing breaks by keeping them.
5. Run it as a service so it survives reboots:
   `sudo cloudflared service install` then `sudo systemctl enable --now cloudflared`.

Keep the compose port mapping as `127.0.0.1:3131:3131`. Do not change it to
`0.0.0.0`. `cloudflared` runs on the same host and reaches loopback fine, and
leaving it on loopback keeps the container off the LAN and public internet.

## 3. Supabase OAuth config

The browser runs Google OAuth with `redirectTo: window.location.origin`, so the
live origin must be authorized or login will loop.

1. Supabase dashboard, Authentication, URL Configuration:
   - Site URL: `https://siftroles.com`
   - Redirect URLs: add `https://siftroles.com` and `https://app.siftroles.com`
     (the staging project adds `https://staging.siftroles.com`), plus any local
     dev origin you still use, e.g. `http://localhost:3131`. Keep the legacy
     `https://anitajob.xyz` entries while that alias is still live.
2. Supabase dashboard, Authentication, Sign In / Providers, Google:
   - Enable the Google provider.
   - Set the Google OAuth client ID and client secret for this Supabase project.
3. Google Cloud Console, OAuth client:
   - Authorized redirect URIs must include the Supabase callback:
     `https://<ref>.supabase.co/auth/v1/callback`.

Repeat the provider setup for every Supabase project. A staging project cloned
from prod settings still starts with providers disabled unless you enable and
configure Google there too; otherwise Supabase returns
`Unsupported provider: provider is not enabled`.

## 4. Verify

1. `docker compose up -d --build`, then confirm health locally:
   `curl -s http://localhost:3131/healthz` returns `{"ok":true}`.
2. Through the tunnel: open `https://siftroles.com/healthz` from your phone on
   cellular (not the home Wi-Fi) and confirm the same JSON.
3. Sign in with a Google account, complete onboarding (resume, profile,
   targets), and confirm a scrape runs and jobs get scored.

## 5. Defense-in-depth: Postgres RLS (optional, deliberate flip)

Tenant isolation is enforced in the app today: every query is scoped with
`WHERE user_id = ?` (proven by `internal/db/isolation_pg_test.go`). RLS is a
*backstop*: it makes an accidental unscoped query fail closed (return zero rows)
instead of leaking another tenant's data. The policies already ship dormant in
`internal/db/migrations/postgres/00002_rls.sql`; activating them is an env flip
plus a restricted DB role. It is **not** required for a small friends launch.

How it works: with `RLS_ENFORCE=true`, the dashboard serves each request through a
restricted (non-BYPASSRLS) role inside a transaction that first runs
`set_config('app.user_id', '<sub>', true)`, which the policies match `user_id`
against. The background crons keep using the service role (`DATABASE_URL`) because
they need cross-tenant reads and DDL.

1. **Create the restricted role.** Run `scripts/create-app-rls.sql` as the owner/
   service role (`postgres`), passing a strong random password as a psql variable so
   it is never written into the repo. On the self-hosted stack:
   ```bash
   APP_RLS_PW="$(openssl rand -hex 24)"
   docker exec -i supabase-db-prod \
     psql -U postgres -d postgres -v ON_ERROR_STOP=1 -v pw="$APP_RLS_PW" \
     -f - < scripts/create-app-rls.sql
   # put $APP_RLS_PW into RLS_DATABASE_URL in .env (below); do not commit it
   ```
   On hosted Supabase, paste the same statements into the SQL editor instead. The
   role gets DML only, never DDL, so it cannot run migrations — that is why the
   service role still does. Because `app_rls` is not the table owner, the dormant
   `ENABLE` (not `FORCE`) policies apply to it automatically.

2. **Migrate first, then flip.** Make sure the schema is current under the service
   role: `docker compose up -d --build` once with `RLS_ENFORCE` unset (the
   service-role connection runs any pending migrations). Then set in `.env`
   (self-hosted, co-located DB — matches `DATABASE_URL`'s host so the dashboard read
   path stays on the local Docker network and adds no pooler egress):
   ```
   RLS_ENFORCE=true
   RLS_DATABASE_URL=postgresql://app_rls:<pw>@supabase-db-prod:5432/postgres?sslmode=disable
   ```
   (On hosted Supabase the role name is `app_rls.<project-ref>` and the host is the
   pooler on `:6543` with `sslmode=require`.) Then `docker compose up -d` again. On
   boot the logs show `RLS enforcement active for dashboard request path`; if instead
   you see `RLS serving repo open failed`, the DSN or role is wrong and it has fallen
   back to the service role.

3. **Validate.** Sign in and confirm the dashboard still lists/scores your own jobs
   (app-layer scoping unchanged). Then, as `app_rls` in psql, confirm a deliberately
   unscoped read fails closed:
   ```sql
   set app.user_id = 'some-other-sub';
   select count(*) from jobs;   -- only that sub's rows; 0 if it owns none
   reset app.user_id;
   select count(*) from jobs;   -- 0 rows: GUC unset => policy denies everything
   ```

4. **Rollback** is just removing `RLS_ENFORCE` (and restarting); the policies go back
   to dormant and the dashboard serves through the service role again.

## Notes and later hardening

- Every tenant scores on the shared host key (`GEMINI_API_KEY`). The setup
  wizard's key step is a no-op in hosted mode (never writes the shared `.env`), so
  a signup can never overwrite your host key. Per-tenant volume is bounded by
  `GEMINI_HOST_PER_TENANT_DAILY_LIMIT`.
- Per-tenant request rate limiting (section 1, `RATE_LIMIT_PER_MINUTE`) throttles the
  expensive endpoints (market research, ask, scrape-now, profile refresh/extract) so
  one friend can't hammer the host key or CPU. It bounds request rate; the daily
  Gemini quota bounds call volume. They are independent guards.
- Still open for a wider, fully public opening later: a Cloudflare Turnstile/captcha
  step on signup, and an email allowlist on the auth path. Neither is required for a
  small invite-only friends launch.
