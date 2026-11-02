# Communicating during an outage

Customers forgive outages faster than silence. A status page update that says the team knows, what is affected and when the next update lands is worth more than a technically perfect diagnosis delivered an hour late. Commit to an update cadence — every thirty or sixty minutes depending on severity — and keep it even when the update is that nothing changed; the absence of an update reads as abandonment.

A template keeps updates honest under pressure: what is broken, who it affects, what we know, what we are doing and the time of the next update. Say the confirmed, not the hoped; do not announce root causes you have not verified, because retracting a wrong cause costs more trust than admitting uncertainty. Avoid internal jargon and blame language — database replica exhausted connections, not the database team made a mistake.

Internal communication runs on a dedicated incident channel or bridge with a pinned summary, so anyone joining sees current state instead of scrolling. Assign one person to post the customer-facing updates and one to answer stakeholder questions inside the company; responders debugging should not be drafting tweets. When service is restored, post the resolution promptly, summarize the impact window honestly, and point to the upcoming incident report rather than narrating the cause in the status page itself.
