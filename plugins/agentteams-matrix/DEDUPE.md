# Matrix source ownership

Core owns the Matrix contracts and the shared Python implementation under
`core/`. The QwenPaw Harness owns its native Matrix channel and receipt
middleware. The managed QwenPaw image installs that adapter and includes the
shared Matrix core alongside it.
