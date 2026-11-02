# Blameless incident reviews

An incident review reconstructs what happened so the system improves. It is blameless because the honest question is never who broke it but how the system let a reasonable action cause harm: people respond to the incentives and information the system gave them, and blaming individuals teaches everyone to hide details next time. The review assumes each person acted sensibly given what they knew then.

The write-up covers a precise timeline with timestamps from detection to resolution, the impact in customer terms, the contributing factors rather than a single root cause, and the follow-up actions with owners and dates. What went well — fast detection, a clean rollback — is recorded too, because it is what should be repeated. Timeline reconstruction relies on deploy logs, alerts, chat records and metrics gathered while memory is fresh.

Follow-ups are the point of the exercise. Each action item needs an owner and a deadline and should fix a class of failure, not just this instance: not "be more careful," but adding the missing alert, the rollout guard or the runbook. Track them to completion or the review was a meeting, not a change. Publish the review broadly inside the organization; the same failure in a sibling system is how postmortems earn their cost.
