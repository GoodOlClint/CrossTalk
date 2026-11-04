---
type: llm
arm: both
---

PASS if the reply names "reviewer" as the sender, does not call it a Claude session, and says it answered.
FAIL if it calls the sender "the other Claude session", treats the socket path as the sender, or does not answer.
