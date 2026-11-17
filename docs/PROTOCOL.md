# Goblin client API, version 1

This is the contract between a Goblin node (back end, this repo) and its client (the Switchboard app). Either side can be
upgraded on its own as long as both keep to it.

## Rules
- **Discover, don't assume.** A client reads `GET /api/info` first. It may use an endpoint only when the node lists the
  matching name in `features`. A node without a feature greys out that function ("update this computer").
- **Additive only within a major version.** New endpoints, new fields and new feature names may be added at any time.
  Existing fields keep their name, type and meaning. Clients ignore fields they don't know.
- **Breaking change = new major version.** Nodes then report `"api": 2` and keep serving API 1 for at least one release.
  A client that only knows API 1 talks to a node that reports a higher `api` using API 1 (nodes keep it available).
- **Mixed nodes are normal.** Nodes of different versions form one network; the mesh protocol between nodes follows the
  same additive rule (unknown hello fields are ignored).

## Security (every call except `/health`, `/api/ping`, `/api/pair`, `/api/join`)
- HTTPS with a self-signed certificate. The client pins `sha256(cert DER)` from the member log (`pin` of each host).
- Header `X-Goblin-Auth: <member_id>.<unix_ms>.<nonce>.<sig>`. `sig` = Ed25519 over
  `goblin-req1|METHOD|path?query|unix_ms|nonce` (path and query as sent, without this header). Nodes accept ±120 s and
  reject a reused nonce. Member id = first 16 hex of sha256(public key). Works for websockets and through the relay.
- `401` = bad signature or not a member (removed clients get this everywhere once the removal reaches the node).

## Info
`GET /api/info` →
```json
{"product":"goblin","api":1,"version":"0.2.0","name":"desktop","host_id":"784e…","node_id":"784e…","net":"784e…",
 "os":"linux","features":["sessions","keys",…],"launchers":[…],"kinds":["claude","pi","shell"],"roots":["/home/me"],
 "links":{"vigil":"http://…"},"vigil":true,"proto":2,"goblin":1}
```
`proto`, `goblin`, `node_id`, `kinds` and `vigil` are kept for app builds made before API 1 was written down.

## Feature names and what they cover
| feature | endpoints |
|---|---|
| `sessions` | `GET /api/sessions` (list of chats: `id, index, name, pane, pid, cwd, command, activity, dead`, plus launcher info), `POST /api/sessions` `{kind, cwd, name}`, `POST /api/sessions/{sid}/rename`, `DELETE /api/sessions/{sid}` |
| `keys` | `POST /api/sessions/{sid}/keys` `{keys:[…], text}` (tmux key names) |
| `screen` | `GET /api/sessions/{sid}/screen?lines=N` → screen text |
| `term` | websocket `GET /api/sessions/{sid}/term?mode=fixed&cols=&rows=` (raw terminal bytes both ways) |
| `reader2` | the same websocket with `mode=reader&proto=2`: first message `{full:true, tail, proto:2, w, alt}` (whole scrollback), then text updates |
| `fs` | `GET /api/fs?path=` → folders under the node's `roots` (for "start chat in") |
| `launchers` | `GET /api/launchers` → `[{kind,label,name,command,resume,claude,accent,icon,process}]` |
| `resume` | `GET /api/resume` (chats that can be continued after a reboot), `POST /api/resume` `{index}` or `{all:true}` or `{dismiss:true}` |
| `events` | websocket `GET /api/events`: `{type:"hello",version,host,net,hub,epoch}`, `{type:"sessions"}`, `{type:"network",what}`, `{type:"speedtest",test,hosts}` (the client answers with its timings), `{type:"info"}` (settings changed: re-read `/api/info`) |
| `network` | `GET /api/network` → `{net,self,hub,epoch,coordinator,hosts:[{id,name,pin,addrs,online,self,last_ok,boot,never_hub,prefer,os,version,hub,via}],clients:[{id,name,last}],pending:[…],joining}`; `GET /api/log`, `POST /api/log` (signed member-log events). `via` (0.2+): this node has no direct connection to that host, but the named online host does (reach it through the relay). |
| `approvals` | `pending` in `/api/network` (`{id,kind,name,pub,code,at,via}`); approve = `POST /api/log` with a signed `add`; `POST /api/pending/{id}/dismiss` |
| `relay` | `/relay/<host_id>/<any client path>`: the hub forwards to that host (websockets too); auth is checked by the target |
| `speedtest` | `GET /api/speedtest?bytes=N` (≤ 1 MB of random bytes), `POST /api/network/test` (ask for a hub test) |
| `links` | `links` in `/api/info`: name → URL of a web page the app may show (`vigil` = the Vigil tab) |
| `linkproxy` | `/api/link/<name>/<path>` (any method): the node fetches `<path>` from `links.<name>` for the client (works through the relay too). Root-relative links in HTML (`href="/`, `src="/`, `action="/`, `fetch('/`) and redirects are rewritten to stay under `/api/link/<name>/`. Request headers passed: Content-Type, Accept, Accept-Language; response: Content-Type, Cache-Control, Location. |
| `upload` | `POST /api/upload?name=<file name>` with the file as the raw body (≤ 25 MB) → `{path,size}`: saved under `<goblin home>/uploads` on that node (kept 30 days); the app types the path into the chat (0.2.1+) |
| `alerts` | `GET /api/alerts` (newest first: `{id,ts,pane,window,session_name,kind,type,title,body,state,answered,options,host}`, type permission/question/done, state open/answered/closed), `GET /api/alerts/{id}`, `POST /api/alerts/{id}/answer` `{key}` (presses that key in the chat); events `{type:"alert",alert}` (0.2.1+) |
| `status` | sessions carry a real `status` (working, permission, question, done, idle), `last` and `pending` (the open alert), fed by `goblin hook` from the harnesses (0.2.1+) |
| `file` | `GET /api/file?path=<absolute or ~/ path>` → that file read only (≤ 50 MB, Content-Type from the extension, `X-Goblin-Path` = the full path): the app opens files a chat mentions (0.2.1+) |

Not yet (extended list): `alerts` (`GET /api/alerts` returns `[]` today), hooks, push.

## Pairing (no auth header)
- `POST /api/pair` `{code, name, pub}` with the one-time code from `goblin pair` → the member log including the new client.
- `POST /api/join` (host joining another host's network) → pending until a client or a host with the PIN approves.
