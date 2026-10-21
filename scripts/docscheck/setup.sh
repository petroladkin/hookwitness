# Setups for docscheck: what a documentation page tells the reader to do in
# the UI before an example, done through the API. A block names them with
# <!-- docs-check: setup: <name> -->; each is the function setup_<name>.
# $HW is the server, $__api its API.

J='Content-Type: application/json'

# hw_endpoint PATH: creates the endpoint unless it exists; prints its id.
hw_endpoint() {
  local id
  id=$(curl -fs "$__api/endpoints?path=$1" | jq -r '.endpoints[0].id // empty')
  [ -n "$id" ] || id=$(curl -fs -X POST "$__api/endpoints" -H "$J" -d "{\"path\":\"$1\"}" | jq .id)
  echo "$id"
}

# hw_secret ID NAME KIND VALUE
hw_secret() {
  curl -fs -o /dev/null -X PUT "$__api/endpoints/$1/secrets/$2" -H "$J" -d "{\"kind\":\"$3\",\"value\":\"$4\"}"
}

# hw_behave ID JQ-FILTER: changes the behaviour document with a jq filter.
hw_behave() {
  local tag doc
  tag=$(curl -fsI "$__api/endpoints/$1/behaviour" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
  doc=$(curl -fs "$__api/endpoints/$1/behaviour" | jq "del(.secrets) | $2") || return 1
  curl -fs -o /dev/null -X PUT "$__api/endpoints/$1/behaviour" -H "$J" -H "If-Match: $tag" -d "$doc"
}

# hw_check ID CHECK-JSON: adds a check.
hw_check() { hw_behave "$1" ".checks += [$2]"; }

# --- getting started, endpoints ---------------------------------------------

setup_stripe_endpoint() { hw_endpoint /stripe/webhooks >/dev/null; }

# --- replies -----------------------------------------------------------------

setup_every_third() {
  local id; id=$(hw_endpoint /billing/notify)
  hw_behave "$id" '.rules = [{"when":{"kind":"every","n":3},"do":"reply","reply":{"status":500}}]'
}
setup_first_two() {
  local id; id=$(hw_endpoint /billing/retry)
  hw_behave "$id" '.rules = [{"when":{"kind":"first","n":2},"do":"reply","reply":{"status":503,"headers":[{"name":"Retry-After","value":"2"}]}}]'
}
setup_slow() {
  local id; id=$(hw_endpoint /billing/slow)
  hw_behave "$id" '.default = {"status":200,"delay_ms":2000}'
}
setup_hang() {
  local id; id=$(hw_endpoint /billing/hang)
  hw_behave "$id" '.rules = [{"when":{"kind":"every","n":1},"do":"timeout"}]'
}
setup_crash() {
  local id; id=$(hw_endpoint /billing/crash)
  hw_behave "$id" '.rules = [{"when":{"kind":"every","n":1},"do":"drop"}]'
}
setup_challenge() {
  local id; id=$(hw_endpoint /slack/events)
  hw_behave "$id" '.default = {"status":200,"headers":[{"name":"Content-Type","value":"application/json"}],"body":"{\"challenge\":\"{{.JSON \"challenge\"}}\"}"}'
}
setup_api_key() {
  local id; id=$(hw_endpoint /partner/hooks)
  hw_behave "$id" '.checks = [{"header":"X-Api-Key","match":"equals","value":"key_test","error":{"status":401,"headers":[{"name":"Content-Type","value":"application/json"}],"body":"{\"error\":\"bad key\"}"}}]'
}

# --- authorization -----------------------------------------------------------

setup_hooks() { hw_endpoint /hooks >/dev/null; }
setup_github() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" GH_SECRET key gh_test_secret && hw_behave "$id" '.checks = [{"kind":"github","secret":"GH_SECRET"}]'
}
# docs/replay.md: a second endpoint that checks the same GitHub signature,
# standing in for the reader's handler.
setup_handler() {
  local id; id=$(hw_endpoint /handler)
  hw_secret "$id" GH_SECRET key gh_test_secret && hw_behave "$id" '.checks = [{"kind":"github","secret":"GH_SECRET"}]'
}
setup_stripe() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" STRIPE_SECRET key whsec_test_stripe && hw_behave "$id" '.checks = [{"kind":"stripe","secret":"STRIPE_SECRET"}]'
}
setup_standard() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" SW_SECRET key whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw && hw_behave "$id" '.checks = [{"kind":"standard","secret":"SW_SECRET"}]'
}
setup_hmac() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" HMAC_KEY key hmac_test_key &&
    hw_behave "$id" '.checks = [{"kind":"hmac","secret":"HMAC_KEY","header":"X-Signature","algo":"sha512","encoding":"base64"}]'
}
setup_bearer() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" API_TOKEN token tok_test_123 && hw_behave "$id" '.checks = [{"kind":"bearer","secret":"API_TOKEN"}]'
}
setup_basic() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" QA_PASSWORD token pw_test && hw_behave "$id" '.checks = [{"kind":"basic","secret":"QA_PASSWORD","user":"qa"}]'
}
setup_basic_enforced() {
  local id; id=$(hw_endpoint /hooks)
  hw_secret "$id" QA_PASSWORD token pw_test && hw_behave "$id" '.checks = [{"kind":"basic","secret":"QA_PASSWORD","user":"qa","enforce":true}]'
}
setup_secret_only() {
  local id; id=$(hw_endpoint /ping)
  hw_secret "$id" PING_TOKEN token tok_ping
}

# --- schemas -----------------------------------------------------------------

setup_orders() {
  local id; id=$(hw_endpoint /orders/events)
  hw_behave "$id" '.schemas = [{"name":"OrderPaid","text":"{\"type\":\"object\",\"required\":[\"order_id\",\"amount\"],\"properties\":{\"order_id\":{\"type\":\"integer\"},\"amount\":{\"type\":\"integer\",\"minimum\":1},\"paid_at\":{\"type\":\"string\",\"format\":\"date-time\"}}}"}]
    | .checks = [{"kind":"schema","select":{"from":"body","key":"/type","map":[{"value":"order.paid","schema":"OrderPaid"}]}}]'
}

# --- troubleshooting ---------------------------------------------------------

setup_small_body() { :; } # the server of that block runs with its own flags

# --- the API reference -------------------------------------------------------

# setup_api: $API, and an endpoint /orders/events as $ID with its $URL.
setup_api() {
  API=$__api
  ID=$(hw_endpoint /orders/events)
  URL=$(curl -fs "$__api/endpoints/$ID" | jq -r .url)
}
# setup_api_draft: requests 1-6 of other kinds, 7 and 8 shipped orders.
setup_api_draft() {
  setup_api
  local i
  for i in 1 2 3 4 5 6; do curl -fs -o /dev/null -d "{\"type\":\"order.paid\",\"order_id\":$i}" "$URL"; done
  curl -fs -o /dev/null -d '{"type":"order.shipped","order_id":7,"shipped_at":"2026-10-14T09:12:00Z","carrier":"DHL"}' "$URL"
  curl -fs -o /dev/null -d '{"type":"order.shipped","order_id":8,"shipped_at":"2026-10-14T09:40:31Z","carrier":"UPS","tracking":"1Z999"}' "$URL"
}
# setup_api_sender: the app under test delivers {"order":42} to /hooks a
# second after the example starts (the example creates the endpoint).
setup_api_sender() {
  API=$__api
  ( sleep 1; curl -fs -o /dev/null -d '{"order":42}' "$HW/hooks" ) &
}
# setup_api_tasks: the app under test calls back for task 41, then twice
# for task 42, a second after the example starts.
setup_api_tasks() {
  setup_api
  ( sleep 1
    curl -fs -o /dev/null -d '{"taskId":41,"state":"done"}' "$URL"
    curl -fs -o /dev/null -d '{"taskId":42,"state":"done"}' "$URL" ) &
}

# --- replies: raw bytes ---------------------------------------------------------

setup_raw_bytes() {
  local id; id=$(hw_endpoint /legacy/hook)
  hw_behave "$id" '.default = {"status":200,"body_base64":"//5vaw=="}'
}

# --- sharing a setup ---------------------------------------------------------

# setup_payments: the developer's /payments/webhooks: an enforced Stripe
# check over STRIPE_SECRET, and the first two deliveries answered 503.
setup_payments() {
  local id; id=$(hw_endpoint /payments/webhooks)
  hw_secret "$id" STRIPE_SECRET key whsec_test_stripe &&
    hw_behave "$id" '.rules = [{"when":{"kind":"first","n":2},"do":"reply","reply":{"status":503,"headers":[{"name":"Retry-After","value":"2"}]}}]
      | .checks = [{"kind":"stripe","secret":"STRIPE_SECRET","enforce":true}]'
}
# setup_payments_file: that endpoint's setup file, as exported, in the
# current directory (the QA engineer received it with the ticket).
setup_payments_file() {
  cat > payments-webhooks.hookwitness.json <<'JSON'
{
  "hookwitness": 1,
  "endpoints": [
    {
      "path": "/payments/webhooks",
      "default": {"status": 200},
      "rules": [
        {"when": {"kind": "first", "n": 2}, "do": "reply",
         "reply": {"status": 503, "headers": [{"name": "Retry-After", "value": "2"}]}}
      ],
      "checks": [{"kind": "stripe", "enforce": true, "secret": "STRIPE_SECRET"}],
      "vars": [],
      "schemas": [],
      "secrets": [{"name": "STRIPE_SECRET", "kind": "key"}]
    }
  ]
}
JSON
}
