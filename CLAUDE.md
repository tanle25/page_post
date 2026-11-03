# page_post

This repo is the Facebook Page MCP server deployed at https://page-post-pi.vercel.app, which `.mcp.json` registers as `facebook-page` (tools `fbp_*`). The `FBP_AUTH_TOKEN` environment variable must hold the server's secret. Never print it, commit it, or paste it anywhere.

## Posting to Pages

- **Every post needs photos.** Pass public `https://` links in `image_urls` (1-10), never text alone. The hosted server cannot read local files, so `image_paths` is refused. Do not use `fbcdn.net` links; they expire. If there is no usable image link, stop and ask.
- **Publish immediately** unless the user asks for a time; only then set `scheduled_time`.
- **Before posting, check the target.** Look up the Page in `fbp_list_pages`, then run `fbp_history` with that `page_id` so the same text does not go out twice.
- **Never post the same text to many Pages.** Write a distinct version for each Page, space posts out by hours, and keep the safe wording of the `fb-content-tre-thu-cong` skill.
- **Stop on a write stop.** If a tool returns `write_stopped`, `token_invalid`, `rate_limited` or `feature_blocked`, stop posting and report it. `fbp_status` shows the cause.
- **Confirm before acting.** Unless the user explicitly delegated it, confirm the Page, text and images before `fbp_create_post`, and confirm before `fbp_delete_post` or `fbp_delete_comment`.

## Accounts

New Facebook accounts are connected by the owner in a browser at https://page-post-pi.vercel.app/connect, not through tools. `fbp_list_accounts` shows the connected accounts, and `fbp_list_pages` with `account_id` shows one account's Pages.

## Deploying

Run `sh deploy.sh`. It deploys the committed files without git metadata, because Vercel rejects the commit email otherwise.
