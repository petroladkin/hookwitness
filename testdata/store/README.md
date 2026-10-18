Databases made by released versions, to test that a newer server opens them
and keeps their data. Synthetic data. Protected: change only with the task's
acceptance.

`v0.1.0.db`: made by `hookwitness serve` 0.1.0 with
`HOOKWITNESS_SECRET_KEY` = base64 of `fixture-key-0.1.0-hookwitness-32`
(a test key), then `PRAGMA journal_mode=DELETE; VACUUM`. It holds the
endpoint `/orders` (id 1) with a secret `hook` (kind key, value
`fixture-signing-key`), an HMAC check on `X-Sig`, a rule "the first 1 →
503", the default reply 202 `ok {{.N}}`; three signed requests to it
(counter 3) and one request to `/nowhere` in Unmatched.
