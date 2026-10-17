Scenario fixtures for internal/schema (JSON Schema checks): schemas, a body, and the expected
verdict or compile error. Synthetic data. Protected: change only with the task's acceptance.

`images.json`: the same shape plus the images each case must record (path,
type, pixel size, the error said); the bodies carry small generated
pictures as base64 or data URLs.

`alternatives.json`: schema checks with several schemas (any of) and a
discriminator's default: the endpoint's schemas, then cases of a check as
the behaviour document writes it, a body and headers, and the verdict
expected (result, reason or its start, the paths of the problems).

`drafts.json`: schema drafts from caught bodies: the bodies (as sent), and
the draft expected (compared as JSON) or the start of the error.
