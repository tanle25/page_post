# page_post: Facebook Page MCP server

MCP server for Facebook **Pages** built on the official Graph API (`graph.facebook.com`). It uses a Meta app and access tokens only: no cookies, no browser, no facebook.com web requests.

## Setup

1. Go to <https://developers.facebook.com/apps>, create an app of type **Business**, and note its **App ID** and **App Secret** (App settings > Basic).
2. Open **Graph API Explorer**, select the app, choose **Get User Access Token**, and grant these permissions:
   - `pages_show_list`
   - `pages_read_engagement`
   - `pages_manage_posts`
   - `pages_manage_engagement`
   - `read_insights`

   Then copy the token.
3. Create `~/.facebook-go/page-config.json` with mode 600. `page-config.example.json` shows the format:
   ```json
   {"app_id": "…", "app_secret": "…", "user_token": "…"}
   ```
4. In Claude Code, run `fbp_refresh_pages`. It does three things:
   - Exchanges the short-lived user token for a long-lived one and writes it back to the config.
   - Stores a token for every Page you manage in `~/.facebook-go/pages.json`.
   - Page tokens derived from a long-lived user token do not expire.
5. Run `fbp_status` to confirm the setup.

The app can stay in Development mode as long as you only manage Pages where you are an admin of the app. App Review is needed only for Pages of people who are not app roles.

`FBP_CONFIG` overrides the config path. `graph_version` in the config overrides the default `v23.0`.

## Deploy to Vercel

The server runs on Vercel's Go runtime as a stateless Streamable HTTP MCP endpoint at `/api/mcp`. State moves to Upstash Redis because Vercel's filesystem does not persist. `vercel.json` selects the Go preset; Vercel builds `main.go` at the repo root.

1. Import the repo in Vercel, or run `vercel` from the repo root. The region is set to `sin1` (Singapore).
2. Under **Storage**, connect an **Upstash Redis** database from the Marketplace. It sets `KV_REST_API_URL` and `KV_REST_API_TOKEN`; `UPSTASH_REDIS_REST_URL` and `UPSTASH_REDIS_REST_TOKEN` also work.
3. Add these environment variables (Production):

   | Variable | Value |
   |---|---|
   | `FBP_AUTH_TOKEN` | A random secret of at least 24 characters, e.g. `openssl rand -hex 32`. Every MCP request must send `Authorization: Bearer <it>`. |
   | `FB_APP_ID`, `FB_APP_SECRET` | From the Meta app |
   | `FB_USER_TOKEN` | User token from Graph API Explorer |
   | `FB_GRAPH_VERSION` | Optional; default `v23.0` |
   | `FBP_OWNER_NAME`, `FBP_CONTACT_EMAIL` | Owner name and contact email shown on `/privacy` and `/data-deletion` |

4. Redeploy. Opening `https://<project>.vercel.app/` should return the server name. Use the production domain: generated preview URLs sit behind Vercel's deployment protection.
5. Point Claude Code at the endpoint. On Claude Code on the web, put this in the repo's `.mcp.json` and set `FBP_AUTH_TOKEN` in the cloud environment's variables:
   ```json
   {"mcpServers": {"facebook-page": {
     "type": "http",
     "url": "https://<project>.vercel.app/api/mcp",
     "headers": {"Authorization": "Bearer ${FBP_AUTH_TOKEN}"}
   }}}
   ```
6. Run `fbp_refresh_pages` once. The long-lived user token and the Page tokens are then stored in Redis. If you later replace `FB_USER_TOKEN`, the stored long-lived token is ignored until you run `fbp_refresh_pages` again.

Hosted-mode differences:

- **Photos.** Photos are passed as public links in `image_urls`; Facebook downloads them. `image_paths` is refused because the server cannot see your files.
- **Write stop.** The write stop is stored in Redis, so every instance honours it.
- **Time limit.** Each tool call is capped at 280 s, under Vercel's 300 s limit.

## Tools

| Tool | What it does |
|---|---|
| `fbp_status` | Config, token validity, expiry and scopes, and stored Pages; clears a write stop |
| `fbp_refresh_pages` / `fbp_list_pages` | Fetch and store Page tokens / list the stored Pages |
| `fbp_get_page` | Page profile and follower count |
| `fbp_get_posts` / `fbp_get_scheduled_posts` / `fbp_get_post` | Published posts with counts / scheduled posts / one post |
| `fbp_get_comments` | Comments on a post, or replies to a comment |
| `fbp_page_insights` / `fbp_post_insights` | Insights metrics; metric names follow Meta's current reference |
| `fbp_history` | Local log of Page writes |
| `fbp_create_post` | Upload photos (required, 1-10, as `image_urls` or local `image_paths`) and publish now; schedules only when `scheduled_time` is given |
| `fbp_update_post` / `fbp_delete_post` | Edit the text of / delete a post |
| `fbp_reply_comment` / `fbp_hide_comment` / `fbp_delete_comment` | Moderate comments as the Page |
| `fbp_delete_stored_data` | Erase all stored tokens, Pages and history (`confirm: "DELETE"`) |

## Going Live

The hosted server also serves a public privacy policy at `/privacy` and data deletion instructions at `/data-deletion`. Both are bilingual (Vietnamese and English) and take the owner and contact from `FBP_OWNER_NAME` and `FBP_CONTACT_EMAIL`. Meta requires both URLs before an app can switch to Live mode. Until the app is Live, posts it publishes may be visible only to people with a role on the app. For Pages you manage yourself, Live mode with Standard Access is enough, without App Review. The step-by-step checklist (in Vietnamese) is in [GO-LIVE.md](GO-LIVE.md).

## Behaviour

- **Tokens stay local.** They are never returned by a tool. When `app_secret` is set, every request carries `appsecret_proof`.
- **Every write is logged.** Each attempt, successful or not, is recorded: in `~/.facebook-go/page-history.jsonl` locally, in the Redis list `fbp:history` when hosted (the last 10,000).
- **Writes stop on serious errors.** After an invalid token, a rate limit, a policy block (code 368), or a write whose outcome is unknown, write tools refuse to run. Running `fbp_status` with a valid token clears the stop.
