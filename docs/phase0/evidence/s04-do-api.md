# S04 — DigitalOcean API evidence (Managed Databases, metadata, rate limits, scopes)

Researched 2026-10-07. Primary source: `digitalocean/openapi` at commit `97dca99` (HEAD of `main`,
committed 2026-10-07), cloned with `git clone --depth 1`. Below, `SPEC/` means
`https://github.com/digitalocean/openapi/blob/main/specification/`, and `DB/` means `SPEC/resources/databases/`.

**Fetch failures:** `docs.digitalocean.com` is **blocked by this sandbox's egress proxy** (WebFetch and
curl both returned 403 on the CONNECT). Facts from docs.digitalocean.com pages therefore come from **search-engine
summaries only** and are marked `search-snippet`. Secondary code sources: `digitalocean/doctl`,
`digitalocean/godo`, and `digitalocean/go-metadata`, all shallow clones of `main` on 2026-10-07.

Confidence legend: **quoted** = verbatim from the spec or code; **inferred** = my reading, not stated;
**search-snippet** = from a web-search summary of a docs page I could not open; **UNCONFIRMED** = not verified.

---

## 1. List clusters — `GET /v2/databases`

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Method/path | `GET /v2/databases`, optional query `tag_name` | SPEC/DigitalOcean-public.v2.yaml (L1214-1216); DB/databases_list_clusters.yml | quoted |
| Response key | "a JSON object with a `databases` key" | DB/databases_list_clusters.yml | quoted |
| Scope | `database:read` (`tag_name` filter: "Requires `tag:read` scope") | DB/databases_list_clusters.yml; DB/parameters.yml | quoted |
| Cluster fields | `id`, `name`, `engine`, `version`, `semantic_version`, `num_nodes`, `size`, `region`, `status` (creating/online/resizing/migrating/forking), `created_at`, `private_network_uuid`, `tags`, `db_names`, `connection`, `private_connection`, `standby_connection`, `standby_private_connection`, `users`, `maintenance_window`, `project_id`, `rules`, `version_end_of_life`, `version_end_of_availability`, `storage_size_mib`, `metrics_endpoints`, `do_settings`, `ui_connection`, `schema_registry_connection` (`autoscale` exists in the write model only) | DB/models/database_cluster.yml, DB/models/database_cluster_read.yml | quoted |
| VPC | `private_network_uuid`: "the UUID of the VPC to which the database cluster will be assigned" ("Requires `vpc:read` scope"). No field is named `vpc` or `vpc_uuid`. | DB/models/database_cluster.yml | quoted |
| Tags | `tags`: string array, nullable. The read model says "Requires `tag:read` scope". | DB/models/database_cluster_read.yml | quoted |
| Connection object | `uri`, `database`, `host`, `port` (int), `user`, `password`, `ssl` (bool). `user`/`password`: "Requires `database:view_credentials` scope." | DB/models/database_connection.yml | quoted |
| `private_connection` | Same schema as `connection` (`$ref database_connection.yml`) | DB/models/database_cluster.yml | quoted |
| maintenance_window | `day` (e.g. "tuesday"), `hour` ("14:00", UTC, 24h), `pending` (bool, readOnly), `description` (string[], readOnly). The object is nullable. | DB/models/database_maintenance_window.yml | quoted |
| Engine enum | `pg, mysql, redis, valkey, mongodb, kafka, opensearch, advanced_pg, advanced_mysql` | DB/models/database_cluster.yml | quoted |

### Standard vs Advanced: what distinguishes them

| Fact | Value | Source | Confidence |
|---|---|---|---|
| **The distinguishing field is `engine`** | "`advanced_pg` for PostgreSQL Advanced Edition, and `advanced_mysql` for MySQL Advanced Edition. Advanced Edition engines are currently in public preview." A Standard PostgreSQL cluster is `engine: "pg"`. | DB/models/database_cluster.yml | quoted |
| Create text | "PostgreSQL and MySQL Advanced Edition clusters can be provisioned by setting `engine` to `advanced_pg` or `advanced_mysql` ... `advanced_pg` supports 1-, 2-, and 3-node deployments" | DB/databases_create_cluster.yml | quoted |
| No other flag | No `layout`, `edition`, `tier` or `advanced` boolean exists on the cluster object. (`grep -i advanced` matches only the engine enum, the options, the create docs and `*_advanced_config` — the advanced config schemas are unrelated engine settings.) | grep of DB/ | quoted (absence verified by grep) |
| Size slug patterns | The options example for `advanced_pg` lists slugs like `gd-2vcpu-8gb`, `gd-2vcpu-8gb-intel`, `so1_5-2vcpu-16gb`. Standard `pg` uses `db-s-*`, `db-amd-*`, `db-intel-*`. The create example for advanced_pg uses `db-s-2vcpu-4gb`, so **do not infer the edition from the size slug; use `engine`.** | DB/responses/options.yml (~L499-530); DB/databases_create_cluster.yml L124-134 | quoted / inferred |
| Options endpoint | `GET /v2/databases/options` returns keys `options.advanced_pg` and `version_availability.advanced_pg` (regions, versions 16/17/18, layouts) | DB/models/options.yml; DB/responses/options.yml | quoted |
| GA status | The spec still says "public preview". A DigitalOcean blog post dated 2026-09-17 reportedly announces GA. | DB/models/database_cluster.yml; digitalocean.com/blog/introducing-mysql-postgresql-advanced | quoted (spec) / search-snippet (GA) |
| Plan changes | "PostgreSQL and MySQL Plan Changes Beginning 15 October 2026": new Standard clusters are limited to plans of 4 GiB RAM or less, with no standby or read-only nodes. API/Terraform/doctl users "may need to update" code. | https://docs.digitalocean.com/release-notes/upcoming/dbaas-plan-changes/ | search-snippet |

## 2. Get one cluster — `GET /v2/databases/{database_cluster_uuid}`

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Method/path | `GET /v2/databases/{database_cluster_uuid}` (also `DELETE`) | SPEC/DigitalOcean-public.v2.yaml L1220-1224 | quoted |
| Response | "a JSON object with a database key", containing the standard cluster attributes (same model as item 1) | DB/databases_get_cluster.yml | quoted |
| Scope | `database:read` | DB/databases_get_cluster.yml | quoted |

## 3. Trusted sources / firewall — `/v2/databases/{uuid}/firewall`

| Fact | Value | Source | Confidence |
|---|---|---|---|
| GET | `GET /v2/databases/{database_cluster_uuid}/firewall` returns `{"rules":[...]}`. Scope `database:read`. | DB/databases_list_firewall_rules.yml | quoted |
| PUT | `PUT /v2/databases/{database_cluster_uuid}/firewall`, body `{"rules":[{type,value,description?,uuid?}]}`. Returns **204 No Content**. Scope **`database:update`**. | DB/databases_update_firewall_rules.yml | quoted |
| Rule schema | `uuid` (string, optional), `cluster_uuid` (readOnly), `type` enum `droplet, k8s, ip_addr, tag, app` (required), `value` (required), `created_at` (readOnly), `description` (optional) | DB/models/firewall_rule.yml | quoted |
| Limits | "The firewall is limited to 100 rules (or trusted sources). You cannot add IPv6 addresses as trusted sources." | DB/databases_update_firewall_rules.yml | quoted |
| Droplet value | Example `{"type":"droplet","value":"163973392"}` (numeric droplet ID as a string) | same | quoted |
| **Replace semantics: the API reference** | The API reference **does not use the words "replace" or "overwrite"**. It says only: "To update a database cluster's firewall rules ... send a PUT request ... specifying which resources should be able to open connections to the database." | DB/databases_update_firewall_rules.yml | quoted (absence verified by grep) |
| **Replace semantics: official CLI** | doctl `databases firewalls replace`: "Replaces the firewall rules for a given database. The rules passed to the `--rules` flag replace the firewall rules previously assigned to the database". doctl `append` is implemented as read, merge, write: "Retrieve any existing firewall rules so that we don't destroy existing rules in the create request." It then calls `UpdateFirewallRules` with the old rules plus the new rule. | https://github.com/digitalocean/doctl/blob/main/commands/databases.go (~L2396, L2512-2565) | quoted (official DO client) |
| **Conclusion** | PUT is a **full replacement of the rule set**. A client must GET, merge and PUT, and it must re-send every existing rule. doctl re-sends `type`, `value`, `cluster_uuid` and `uuid` but **drops `description`**, so our tool should preserve `description` itself. There is no ETag or If-Match, so concurrent edits can race (last write wins). | inferred from the above | inferred (strong) |

## 4. Maintenance window

| Fact | Value | Source | Confidence |
|---|---|---|---|
| GET endpoint | **None.** The spec defines only `put` under `/v2/databases/{database_cluster_uuid}/maintenance`. Read the window from the cluster object (`maintenance_window`). | SPEC/DigitalOcean-public.v2.yaml L1266-1268 | quoted |
| PUT | `PUT /v2/databases/{database_cluster_uuid}/maintenance`, body `{"day":"tuesday","hour":"14:00"}` (both required). Returns 204. Scope `database:update`. | DB/databases_update_maintenanceWindow.yml; DB/models/database_maintenance_window.yml | quoted |
| Related | `PUT /v2/databases/{uuid}/install_update` starts pending maintenance | SPEC/DigitalOcean-public.v2.yaml L1270-1272 | quoted (path only) |

## 5. Storage size

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Field | `storage_size_mib` (integer, **MiB**), e.g. 61440 = 60 GiB | DB/models/database_cluster.yml | quoted |
| Semantics caveat | Model text: "Additional storage added to the cluster, in MiB. If null, no additional storage is added to the cluster, beyond what is provided as a base amount from the 'size'". The create, resize and example payloads, however, send totals (61440, 163840). **Whether the GET value is the total disk or only the extra above the plan base is ambiguous. Verify against a live cluster.** | DB/models/database_cluster.yml; DB/databases_update_clusterSize.yml | quoted / UNCONFIRMED |
| Change | `PUT /v2/databases/{uuid}/resize` with `size`, `num_nodes`, optional `storage_size_mib`. Returns 202 and status `resizing`. | DB/databases_update_clusterSize.yml | quoted |
| Used disk | The cluster object has no "used bytes" field. That data would need metrics or SQL. | grep of DB/models | inferred |

## 6. Databases inside a cluster — `/v2/databases/{uuid}/dbs`

| Fact | Value | Source | Confidence |
|---|---|---|---|
| List | `GET /v2/databases/{database_cluster_uuid}/dbs` returns `{"dbs":[{"name":...}]}`. Scope `database:read`. | DB/databases_list.yml; DB/models/database.yml | quoted |
| Create | `POST /v2/databases/{database_cluster_uuid}/dbs`, body `{"name":"alpha"}`. Returns **201** with `{"db":{...}}`. Scope `database:create`. | DB/databases_add.yml | quoted |
| Get/Delete | `GET` / `DELETE /v2/databases/{uuid}/dbs/{database_name}` | SPEC/DigitalOcean-public.v2.yaml L1322+ | quoted (paths) |
| Exclusions | "Database management is not supported for Caching or Valkey clusters." Advanced engines are **not** listed as excluded. | DB/databases_add.yml | quoted |
| Advanced: API or SQL? | **UNCONFIRMED.** The spec has no Advanced-specific caveat for `/dbs`. I could not open the Advanced Edition how-to page (egress blocked), and search snippets did not cover database/user management. | — | UNCONFIRMED |
| Alt. view | The cluster object's `db_names` (readOnly string[]) | DB/models/database_cluster.yml | quoted |

## 7. Users — `GET /v2/databases/{uuid}/users`

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Endpoint | `GET /v2/databases/{database_cluster_uuid}/users` returns `{"users":[...]}`. Scope `database:read`. Also `POST` (add) and `GET/PUT/DELETE .../users/{username}`. | DB/databases_list_users.yml; SPEC/...v2.yaml L1298-1306 | quoted |
| Passwords | "User passwords will not show without the `database:view_credentials` scope." The user model's `password` says "Requires `database:view_credentials` scope." | DB/databases_list_users.yml; DB/models/database_user.yml | quoted |
| User fields | `name`, `role` (`primary`/`normal`), `password`, `access_cert`/`access_key` (Kafka), `mysql_settings`, `settings` (PG: `pg_allow_replication`) | DB/models/database_user.yml; DB/models/user_settings.yml | quoted |

## 8. Droplet metadata service (`http://169.254.169.254/metadata/v1/`)

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Base | `http://169.254.169.254/metadata/v1/` (index). `/metadata/v1.json` returns everything as one JSON document. | go-metadata `client.go` (`defaultPath = "/metadata/v1/"`, `resolve("/metadata/v1.json")`) | quoted (code) |
| Droplet ID | `/metadata/v1/id` (JSON key `droplet_id`) | go-metadata client.go, client_test.go, all_json.go | quoted (code) |
| Hostname / region / tags | `/metadata/v1/hostname`, `/metadata/v1/region` (e.g. `nyc3`), `/metadata/v1/tags` | go-metadata client_test.go | quoted (code) |
| Public IPv4 | `/metadata/v1/interfaces/public/0/ipv4/address`. The JSON shape is `interfaces.public[0].ipv4.ip_address`. Private Droplets have no `interfaces/public/`. | go-metadata all_json.go (JSON); docs access-metadata page | quoted (JSON) / search-snippet (path) |
| Private (VPC) IPv4 | `/metadata/v1/interfaces/private/0/ipv4/address` (the docs call it the Droplet's "VPC IP address"). JSON: `interfaces.private[0].ipv4.ip_address`. | https://docs.digitalocean.com/products/droplets/how-to/access-metadata/ ; https://docs.digitalocean.com/reference/api/metadata/network-interfaces/ | search-snippet |
| Reserved IP | `/metadata/v1/reserved_ip/ipv4/active` (`floating_ip/...` is the legacy path) | go-metadata client.go | quoted (code) |
| VPC UUID | **UNCONFIRMED / likely absent.** go-metadata's `Metadata` struct has no VPC ID field (only `features.vpc_peering_enabled`). Get the droplet's `vpc_uuid` from the API instead (`GET /v2/droplets/{id}` with the ID from the metadata service). | go-metadata all_json.go | inferred |

## 9. Rate limits

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Limits | "5,000 requests per hour", "250 requests per minute (5% of the hourly total)", "rate limited per OAuth token" | SPEC/description.yml (§Rate Limit, ~L189-196) | quoted |
| Headers | `ratelimit-limit`, `ratelimit-remaining`, `ratelimit-reset` (Unix epoch, "the time when the _oldest_ request will expire"). Each request expires on its own one-hour timer (sliding window). | SPEC/description.yml; SPEC/shared/headers.yml | quoted |
| 429 | "If the `ratelimit-remaining` reaches zero, subsequent requests will receive a 429 error code". Body `{"id":"too_many_requests","message":"API rate limit exceeded."}`. | SPEC/description.yml; SPEC/shared/responses/too_many_requests.yml | quoted |
| Retry-After | "**retry-after**: The number of seconds to wait ... More rate limiting information is returned only within burst limit error response headers". It is only on 429s from the **per-minute burst** limit: "the 429 error response will include a `retry-after` header". Per-endpoint `too_many_requests.yml` lists only the 3 ratelimit-* headers. | SPEC/description.yml (~L207-235) | quoted |
| Client policy | Honor `retry-after` when present. Otherwise wait until `ratelimit-reset`, with jittered backoff. | — | inferred |

## 10. Token scopes (custom scopes)

| Fact | Value | Source | Confidence |
|---|---|---|---|
| Database scopes in spec | `database:read`, `database:create`, `database:update`, `database:delete`, `database:view_credentials` (no other `database:*` scopes appear) | grep of DB/ `security:` blocks | quoted |
| Read clusters / firewall / users / dbs | `database:read` | DB/databases_list_clusters.yml, databases_get_cluster.yml, databases_list_firewall_rules.yml, databases_list_users.yml, databases_list.yml | quoted |
| Read credentials | `database:view_credentials` (connection `user`/`password`, user passwords) | DB/models/database_connection.yml; DB/databases_list_users.yml | quoted |
| Update trusted sources | **`database:update`**. This is *not* `firewall:update`, which is the Cloud Firewalls scope. | DB/databases_update_firewall_rules.yml | quoted |
| Maintenance window PUT | `database:update` | DB/databases_update_maintenanceWindow.yml | quoted |
| Create DB in cluster | `database:create` | DB/databases_add.yml | quoted |
| Field-level extras | `private_network_uuid` → `vpc:read`; `tags` → `tag:read`; `project_id` → `project:read` (read model) | DB/models/database_cluster_read.yml | quoted |
| Required companion scopes | The scopes docs reportedly say that `database:update` and `database:view_credentials` each require `database:read`, `regions:read`, `sizes:read` and `actions:read`. | https://docs.digitalocean.com/reference/api/scopes/ ; .../scopes/database/update ; .../scopes/database/view_credentials | search-snippet |
| Minimal token for our tool | `database:read`, `database:view_credentials`, `database:update` (+ `database:create` if we create DBs via API), plus `regions:read`, `sizes:read`, `actions:read`, `vpc:read`, `tag:read`, and `droplet:read` to look up a droplet's `vpc_uuid` | — | inferred |

## Open items / not confirmed
1. Whether `storage_size_mib` on GET is the total or only the additional storage. Verify on a live cluster.
2. Whether `POST /dbs` and `POST /users` are supported, or recommended, for `advanced_pg`. Not stated in the spec. The Advanced Edition doc page could not be fetched.
3. The Advanced Edition GA date (blog, 2026-09-17), and whether the spec's "public preview" wording is stale.
4. The exact metadata paths for public/private IPv4 come from search snippets. They match the JSON structure in go-metadata, but I did not read the docs page directly.
5. The scope dependency list (regions/sizes/actions:read) comes from search snippets.
