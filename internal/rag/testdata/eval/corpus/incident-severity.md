# Incident severity levels

A severity classification decides who gets paged and how loudly. It measures impact, not effort: an easy fix to a broken payment flow outranks a hard fix to a cosmetic bug. The usual scheme runs from the highest severity for a service-down or data-loss event affecting most users, down to a minor defect with a workaround affecting few. Define each level with observable criteria — percent of requests failing, customers affected, revenue or compliance exposure — so the classification does not depend on who happens to be awake.

The first five minutes set the tone. Whoever detects the incident declares the severity they believe applies, pages accordingly and writes the first status line. Treat the classification as provisional: a commander reclassifies up or down as the blast radius becomes known. Escalating early costs an interrupted evening; under-declaring a major outage costs far more.

Keep the definitions written where the on-call runbook lives, with concrete examples from past incidents. Disagreements about severity mid-incident are settled by the incident commander, not by committee, and revised at the postmortem rather than during the firefight. Severity drives the communication cadence too — the highest level expects stakeholder updates every half hour — so keep the label visible on the incident channel topic.
