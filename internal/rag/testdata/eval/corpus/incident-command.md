# The incident commander role

An incident commander is a coordinator, not the smartest debugger in the room. The role exists because an outage handled by everyone at once produces three parallel investigations, untracked mitigation attempts and no one noticing that the fix made things worse. The commander holds the current picture: what is believed, what has been tried, what happens next and who owns it.

Concretely, the commander assigns work to individuals by name — not to the channel — sets an explicit order of operations, and decides when to roll back, accept partial service or escalate. They ask the inevitable question of who else needs to be in the call, and they protect the responders doing hands-on work from a stream of status requests by routing every inquiry through themselves or a dedicated liaison. Delegating is the skill: a commander typing fixes into the terminal has stopped commanding.

Handover matters in long incidents. Write down the running state before transferring command — current hypothesis, pending actions, who is paged — so the next commander does not restart the investigation. The commander owns the decision to declare the incident resolved and triggers the postmortem, but the write-up itself belongs to the responders. On small teams the same person may hold both roles; be deliberate about which hat is on at each moment.
