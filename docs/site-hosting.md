# Public website and demo hosting

The product website and the Go control plane are separate deployments.
`site/` contains static HTML, CSS, JavaScript and recorded illustrations.
`.github/workflows/pages.yml` uploads that directory to GitHub Pages. With the
repository's custom domain configured, one Pages deployment serves
`https://getrunkite.com/`; the project URL redirects to that canonical domain.

The public demo Admin is currently offline. Keep
[the Admin URL](https://getrunkite.com/admin/) and existing navigation links.
Its static availability page directs visitors to local setup. Recorded Admin
images and animations are sample-data captures, not live backend activity.

## Publish the website

1. Keep the Pages publishing source set to GitHub Actions. Configure
   `getrunkite.com` as the repository's custom domain before changing DNS.
2. Point the apex and `www` records at GitHub Pages using its current documented
   DNS configuration. Enable HTTPS after the domain check and certificate are
   ready. This repository's custom Actions source does not require a `CNAME`
   file; Pages ignores that file for this publishing mode. Follow the
   [GitHub custom-domain instructions](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site).
3. Run `make test-site`, publish the reviewed commit through the Pages workflow,
   and verify the canonical homepage, documentation, Engineering pages and
   `/admin/` availability route over HTTPS. Confirm that the project URL reaches
   the same site.

GitHub Pages is [static hosting](https://docs.github.com/en/pages/getting-started-with-github-pages/what-is-github-pages).
It cannot run the Go binary, Postgres, Redis or runners, and cannot provide their
SSE, WebSocket or gRPC backend services. Keep the existing
[self-hosted deployment instructions](deployment.md) for those processes.

## Restore the public backend later

The retained `/admin/` URL is the stable entry point. Restore the backend on
infrastructure that can run the plane and its dependencies, then choose either:

- Replace the availability page with a link or static redirect to the future
  backend's Admin hostname. Existing links to `getrunkite.com/admin/` keep
  reaching that entry point.
- Put a path-aware reverse proxy in front of the public hostname. Route Admin
  and its `/admin-api/` requests to the control plane and website paths to the
  static site. Route any intentionally exposed client APIs and streaming paths
  explicitly; provide gRPC through its own supported listener.

DNS chooses a hostname's destination, not a URL path. Changing DNS alone cannot
send `/admin/` to one server while the same hostname's other paths stay on Pages.
Keep availability copy and tooltip text accurate when the backend returns.
