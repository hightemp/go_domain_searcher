# domain_search

Fast domain candidate generator and HTTP liveness checker written in Go. It enumerates label strings, appends TLDs, and probes domains over HTTP/HTTPS with rate limiting and concurrency, writing working domains to per‑TLD files.

## Features
- Generates domain names from a configurable alphabet and length range
- Enforces hyphen rules (no leading/trailing, no double when configured)
- Loads TLDs from a local file or the official IANA list
- High‑throughput HTTP checks with global RPS limit and worker pool
- Live, ETA‑based progress shown on stderr
- Outputs results into `domains/<tld>.txt` files

## Notes
- `generator.tlds_file` may be a local path or HTTP/HTTPS URL. When provided, it overrides `generator.tlds`. If both are empty, startup validation fails.
- HTTP checker:
  - Method defaults to `GET`; `try_https_first` controls scheme order (HTTPS→HTTP).
  - Any status within `[accept_status_min, accept_status_max]` is considered successful.
  - `body_limit` caps the number of response bytes read per request.
- `limits.rate_per_second` applies globally; `limits.concurrency` controls worker count.
- `max_candidates` bounds the number of generated candidates per pass; when `run.loop` is true the program starts a new pass after reaching the limit.

## License

MIT