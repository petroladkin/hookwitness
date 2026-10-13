# Report fixtures

`requests.json` holds synthetic caught requests of one endpoint, written by
hand to cover every way a report shows a request: a passing JSON delivery
and its retry, a body failing its schema (refused, with problems marked),
a timeout with a text body full of backticks and pipes, a dropped binary
body, a body over the limit (not kept), a template error with secret-like
headers to mask, and a body longer than the shown limit.

`report.md` and `report.html` are the expected reports of these requests
with the options in `internal/report/report_test.go`. They are golden
files: after an intended change to a report, regenerate them with
`go test ./internal/report -update` and review the diff.
