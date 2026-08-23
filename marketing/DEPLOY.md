# Deploying the Sift marketing site

Static site (`index.html` + `styles.css`). Hosted on **Cloudflare Pages**, served at
the apex `siftroles.com`. The dashboard is untouched and stays at `app.siftroles.com`.

## First-time deploy

From the repo root:

```bash
# 1. Authenticate (opens a browser once)
npx wrangler login

# 2. Create the project + push the first deploy (direct upload of the marketing/ dir)
npx wrangler pages deploy marketing --project-name=sift-marketing --branch=main --commit-dirty=true
```

This returns a live `https://sift-marketing.pages.dev` URL. Confirm it looks right.

## Point siftroles.com at it

The apex currently 301-redirects to `app.siftroles.com` via a Cloudflare Page Rule /
Redirect Rule. To serve the marketing site instead:

1. Cloudflare dashboard → **Rules** → delete (or disable) the rule that redirects
   `siftroles.com` → `app.siftroles.com`. Leave any `www` handling as-is.
2. Cloudflare dashboard → **Workers & Pages** → `sift-marketing` → **Custom domains**
   → add `siftroles.com` (and `www.siftroles.com` if you want). Cloudflare wires the
   DNS automatically since the zone is already here.
3. Wait for the cert to issue (usually under a minute), then load `https://siftroles.com`.

`app.siftroles.com` is a separate hostname on the Tunnel and is not affected.

## Updating the site later (CI/CD)

Deploys are automated. `.github/workflows/deploy-marketing.yml` runs
`wrangler pages deploy marketing` on every push to `main` that touches
`marketing/**`, and can be triggered by hand from the Actions tab
("Deploy marketing site" → Run workflow). Normal flow: edit under `marketing/`,
PR into `dev`, promote `dev`→`main`, the workflow ships it.

One-time secret setup (only if the workflow reports a missing token):

```bash
# Create a token in Cloudflare → My Profile → API Tokens with the
# "Cloudflare Pages: Edit" permission on the account that owns sift-marketing.
gh secret set CLOUDFLARE_API_TOKEN  -R jakemercure28/job-search-automation-private
gh secret set CLOUDFLARE_ACCOUNT_ID -R jakemercure28/job-search-automation-private
```

Manual fallback (bypasses CI, e.g. secret not set yet):

```bash
npx wrangler pages deploy marketing --project-name=sift-marketing --branch=main --commit-dirty=true
```

## Rollback

Re-enable the apex redirect rule from step 1, and the site reverts to sending
`siftroles.com` traffic to the dashboard.
