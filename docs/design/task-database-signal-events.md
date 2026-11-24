# Task database follow-up: signal events and notifications

The file-based retention change in #1712 is an interim performance measure:
completed projects leave the live stream on a retention sweep; inactive projects
use a seven-day default; queries may trigger a sweep. Archive publication and
source cleanup must remain retryable, and live readers must skip archive payloads.

The future task-management database design must include signal events and
notification records alongside projects and tasks. Preserve the existing
`event_id`, team/project/task/loop attribution, event type, occurrence time,
origin, reference and result fingerprint so consumers retain deduplication and
correlation across the migration. Notification delivery/read state should be
associated with the event and recipient rather than inferred from whether a
live file is still listed.

Plan indexed notification/history queries, stable cursor pagination and filters
for event type, task and notification state. Define retention and archive access
explicitly so historical visibility does not depend on a query-triggered cleanup.
This is a database-design requirement, not a new database or historical-query API
implemented by the temporary file fix.
