#!/usr/bin/env bash
# 
# E2E suite for MCP <=> REST translation
#

set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE="docker compose -p mcp-e2e -f examples/mcp-server/docker-compose.yml"
MCP_URL="http://localhost:10000/mcp"
# Per-request curl budget; a hang shows up as http_code=000 at this limit.
MAX_TIME=8
READINESS_ATTEMPTS=30
FAILED=0

BODY_FILE=$(mktemp)
HDRS_FILE=$(mktemp)

cleanup() {
  code=$?
  [ "$code" -eq 0 ] && [ "$FAILED" -gt 0 ] && code=1
  if [ "$code" -ne 0 ]; then
    $COMPOSE logs --no-color envoy envoy-mcp-openapi-processor prism > e2e/last-run.log 2>&1 || true
    echo "logs captured in e2e/last-run.log"
  fi
  $COMPOSE down -v >/dev/null 2>&1 || true # -v removes the shared-uds volume
  rm -f "$BODY_FILE" "$HDRS_FILE"
  exit "$code"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*"
  FAILED=$((FAILED + 1))
}

pass() { echo "ok:   $*"; }

# request NAME EXPECTED_STATUS [curl args...]
# Sends a request to $MCP_URL, capturing status, headers and body. Fails the
# case on an unexpected status; http_code=000 after --max-time is the
# hang/connection-failure signature.
request() {
  local name=$1 expected=$2 status
  shift 2
  status=$(curl -s -o "$BODY_FILE" -D "$HDRS_FILE" -w '%{http_code}' --max-time "$MAX_TIME" "$@" "$MCP_URL" || true)
  if [ "$status" = "000" ]; then
    fail "$name: no HTTP response within ${MAX_TIME}s (hang or connection failure)"
    return 1
  fi
  if [ "$status" != "$expected" ]; then
    fail "$name: expected HTTP $expected, got $status (body: $(head -c 300 "$BODY_FILE"))"
    return 1
  fi
  return 0
}

# post NAME EXPECTED_STATUS JSON_BODY [curl args...]
post() {
  local name=$1 expected=$2 body=$3
  shift 3
  request "$name" "$expected" -X POST -H 'content-type: application/json' -d "$body" "$@"
}

# assert_jq NAME FILTER 
# the captured body must satisfy the jq filter (its output must be truthy).
assert_jq() {
  local name=$1 filter=$2
  if ! jq -e "$filter" "$BODY_FILE" >/dev/null 2>&1; then
    fail "$name: body does not satisfy jq filter '$filter' (body: $(head -c 300 "$BODY_FILE"))"
    return 1
  fi
  return 0
}

# assert_header NAME HEADER
# the captured response headers must contain HEADER.
assert_header() {
  local name=$1 header=$2
  if ! grep -qi "^${header}:" "$HDRS_FILE"; then
    fail "$name: response is missing header '$header'"
    return 1
  fi
  return 0
}

# bodyless request (end_of_stream on the request headers): must return a
# JSON-RPC Invalid Request error instead of stalling in the body-buffering
# state waiting for a body that never arrives.
case_bodyless_request() {
  local name="bodyless POST /mcp -> JSON-RPC Invalid Request"
  request "$name" 200 -X POST || return 0
  assert_jq "$name" '.error.code == -32600' || return 0
  pass "$name"
}

# GET /mcp falls through to the main route (ext_proc enabled); the
# processor's method guard answers 405 with Allow: POST instead of parsing
# the request as JSON-RPC.
case_get_rejected() {
  local name="GET /mcp -> 405"
  request "$name" 405 || return 0
  assert_header "$name" 'allow' || return 0
  pass "$name"
}

# DELETE /mcp falls through to the main route (ext_proc enabled); the
# processor's method guard answers 405 with Allow: POST instead of parsing
# the request as JSON-RPC.
case_delete_rejected() {
  local name="DELETE /mcp -> 405"
  request "$name" 405 -X DELETE || return 0
  assert_header "$name" 'allow' || return 0
  pass "$name"
}

# CORS preflight is needed for local browser-based testing (MCP inspector)
# and Envoy is configured to skip ext_proc in this case.
case_cors_preflight() {
  local name="OPTIONS /mcp CORS preflight passes through"
  request "$name" 200 -X OPTIONS \
    -H 'Origin: http://localhost:6274' \
    -H 'Access-Control-Request-Method: POST' || return 0
  assert_header "$name" 'access-control-allow-origin' || return 0
  assert_header "$name" 'access-control-allow-methods' || return 0
  pass "$name"
}

# tools/list is answered by the processor itself (immediate-response path).
case_tools_list() {
  local name="tools/list -> non-empty tool list"
  post "$name" 200 '{"jsonrpc":"2.0","id":3,"method":"tools/list"}' || return 0
  assert_jq "$name" '.id == 3 and (.result.tools | length > 0)' || return 0
  pass "$name"
}

# notifications/initialized short-circuits with a plain HTTP 202.
case_notification() {
  local name="notifications/initialized -> HTTP 202"
  post "$name" 202 '{"jsonrpc":"2.0","method":"notifications/initialized"}' || return 0
  pass "$name"
}

# deletePet without credentials: Prism answers 401 with an empty body, which
# the processor must synthesize into a valid MCP error envelope
# (empty-upstream path: end_of_stream on the response headers).
case_empty_upstream_body() {
  local name="tools/call deletePet without credentials (401, empty upstream body) -> synthesized MCP error"
  post "$name" 200 '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"deletePet","arguments":{"petId":1}}}' || return 0
  assert_jq "$name" '.id == 5 and .result.isError == true' || return 0
  assert_jq "$name" '.result.content[0].text | contains("no response body")' || return 0
  pass "$name"
}

# dns-rebinding-protection rejects non-localhost origin
case_rebinding_origin() {
  local name="POST /mcp with Origin: http://evil.com -> 403"
  post "$name" 403 '{"jsonrpc":"2.0","id":8,"method":"tools/list"}' -H 'Origin: http://evil.com' || return 0
  pass "$name"
}

# dns-rebinding-protection accepts localhost origin
case_allowed_origin() {
  local name="POST /mcp with Origin: http://localhost:6274 -> 200 tools"
  post "$name" 200 '{"jsonrpc":"2.0","id":10,"method":"tools/list"}' -H 'Origin: http://localhost:6274' || return 0
  assert_jq "$name" '.id == 10 and (.result.tools | length > 0)' || return 0
  pass "$name"
}

# malformed JSON body: processor answers with a JSON-RPC Parse error.
case_malformed_json() {
  local name="malformed JSON body -> JSON-RPC Parse error"
  post "$name" 200 '{"jsonrpc":' || return 0
  assert_jq "$name" '.error.code == -32700' || return 0
  pass "$name"
}


# scan Envoy + processor logs for silent failures (e.g. ext_proc mutation
# rejections that do not surface as a curl error).
case_log_scan() {
  local name="log scan: no errors/violations in envoy or processor logs"
  local pattern='protocol violation|panic|rejected|local_reply|ERROR'
  local hits
  hits=$($COMPOSE logs --no-color envoy envoy-mcp-openapi-processor 2>&1 | grep -E "$pattern" || true)
  if [ -n "$hits" ]; then
    fail "$name: found suspicious log lines:"
    echo "$hits" | head -20
    return 0
  fi
  pass "$name"
}

# --- Per-tool success coverage ---
#
# Every exposed petstore tool, invoked with valid arguments and credentials,
# must return a successful MCP result (isError omitted / != true). Credentials:
# an `api_key` header satisfies the apiKey schemes (getPetById, getInventory)
# and a bearer token satisfies the petstore_auth oauth2 scheme — Prism accepts
# any value for both. Three success response shapes are asserted:
#   json  — upstream JSON body translated into result.content[0].text, which
#           parses back as an object or array
#   empty — upstream 200 with no body, synthesized into a success envelope that
#           notes "no response body" (same path as case 5, but a 200 so the
#           envelope is a success rather than an error)
#   plain — upstream JSON scalar (loginUser returns a bare string token)
#
# uploadFile is intentionally absent. The processor skips it because its
# request body is application/octet-stream (openapi.go "unsupported request
# body content-type"), so it is not an exposed tool.

# tool_success TOOL KIND ARGS_JSON
tool_success() {
  local tool=$1 kind=$2 args=$3
  local label="tool $tool -> success"
  post "$label" 200 \
    "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}" \
    -H 'api_key: e2e' -H 'Authorization: Bearer e2e' || return 0
  assert_jq "$label" '.id == 1 and .result != null and .result.isError != true' || return 0
  case $kind in
  json) assert_jq "$label" '.result.content[0].text | fromjson | (type == "object" or type == "array")' || return 0 ;;
  empty) assert_jq "$label" '.result.content[0].text | contains("no response body")' || return 0 ;;
  plain) assert_jq "$label" '.result.content[0].text | length > 0' || return 0 ;;
  esac
  pass "$label"
}

case_all_tools_success() {
  tool_success updatePet                json  '{"body":{"name":"doggie","photoUrls":["http://example/p.png"]}}'
  tool_success addPet                   json  '{"body":{"name":"doggie","photoUrls":["http://example/p.png"]}}'
  tool_success findPetsByStatus         json  '{"status":"available"}'
  tool_success findPetsByTags           json  '{"tags":["tag1"]}'
  tool_success getPetById               json  '{"petId":1}'
  tool_success updatePetWithForm        json  '{"petId":1,"name":"rex","status":"sold"}'
  tool_success getInventory             json  '{}'
  tool_success placeOrder               json  '{"body":{"petId":1,"quantity":1}}'
  tool_success getOrderById             json  '{"orderId":1}'
  tool_success createUser               json  '{"body":{"username":"user1"}}'
  tool_success createUsersWithListInput json  '{"body":[{"username":"user1"}]}'
  tool_success getUserByName            json  '{"username":"user1"}'
  tool_success loginUser                plain '{"username":"user1","password":"pw"}'
  tool_success deletePet                empty '{"petId":1}'
  tool_success deleteOrder              empty '{"orderId":1}'
  tool_success logoutUser               empty '{}'
  tool_success updateUser               empty '{"username":"user1","body":{"username":"user1"}}'
  tool_success deleteUser               empty '{"username":"user1"}'
}


echo "=== starting stack (project mcp-e2e) ==="
# Starting only envoy brings up its depends_on chain (processor -> prism); the
# inspector container is not a dependency, so it stays down.
$COMPOSE up --build -d envoy

echo "=== waiting for readiness ==="
# wait_until DESCRIPTION JSON_BODY [curl args...] — polls $MCP_URL with the
# given POST until it returns HTTP 200.
wait_until() {
  local desc=$1 body=$2 status=000
  shift 2
  for _ in $(seq 1 "$READINESS_ATTEMPTS"); do
    status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
      -X POST -H 'content-type: application/json' -d "$body" "$@" "$MCP_URL" || true)
    if [ "$status" = "200" ]; then
      return 0
    fi
    sleep 1
  done
  echo "FAIL: $desc did not become ready within ${READINESS_ATTEMPTS}s (last status: ${status})." \
    "Is port 10000 free and Docker running?"
  exit 1
}
# tools/list returning 200 proves Envoy listener + ext_proc socket +
# processor registry are up; a real tool call additionally proves the Prism
# upstream accepts connections (it starts slower than the rest of the stack).
wait_until "envoy + processor" '{"jsonrpc":"2.0","id":0,"method":"tools/list"}'
wait_until "prism upstream" '{"jsonrpc":"2.0","id":0,"method":"tools/call","params":{"name":"getPetById","arguments":{"petId":1}}}' \
  -H 'api_key: e2e'

echo "=== running probes ==="
case_bodyless_request
case_get_rejected
case_delete_rejected
case_cors_preflight
case_tools_list
case_notification
case_empty_upstream_body
case_rebinding_origin
case_allowed_origin
case_malformed_json
case_log_scan
case_all_tools_success

echo "=== done: $FAILED failure(s) ==="
[ "$FAILED" -eq 0 ]
