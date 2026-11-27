----------------------- MODULE AuthorityCompaction -----------------------
EXTENDS Integers, FiniteSets, TLC
CONSTANTS UnsafeReplay, UnsafeRestore
Nodes == {"A", "B", "C"}
Zones == {0, 1}
MaxSeq == 2
VARIABLES active, installed, consumed, pending, history, messages, crashed, backups, turn
vars == <<active, installed, consumed, pending, history, messages, crashed, backups, turn>>
Init == /\ active = [z \in Zones |-> [holder |-> "A", seq |-> 0]]
        /\ installed = [n \in Nodes |-> IF n = "A" THEN [z \in Zones |-> 0] ELSE [z \in Zones |-> -1]]
        /\ consumed = [n \in Nodes |-> [z \in Zones |-> -1]]
        /\ pending = [z \in Zones |-> [from |-> "A", to |-> "A", seq |-> 0, phase |-> "idle"]]
        /\ history = {}
        /\ messages = {[zone |-> z, from |-> "A", to |-> "A", seq |-> 0] : z \in Zones}
        /\ crashed = {}
        /\ backups = installed
        /\ turn = 0
Begin(z, n) ==
 /\ pending[z].phase = "idle"
 /\ active[z].seq < MaxSeq
 /\ active[z].holder \notin crashed /\ n \notin crashed
 /\ n # active[z].holder
 /\ installed[active[z].holder][z] = active[z].seq
 /\ pending' = [pending EXCEPT ![z] = [from |-> active[z].holder, to |-> n, seq |-> active[z].seq + 1, phase |-> "copy"]]
 /\ UNCHANGED <<active, installed, consumed, history, messages, crashed, backups, turn>>
Fence(z) ==
 LET p == pending[z] IN
 /\ p.phase = "copy" /\ p.from \notin crashed
 /\ consumed' = [consumed EXCEPT ![p.from][z] = p.seq - 1]
 /\ installed' = [installed EXCEPT ![p.from][z] = -1]
 /\ pending' = [pending EXCEPT ![z].phase = "decided"]
 /\ history' = history \cup {[zone |-> z, from |-> p.from, to |-> p.to, seq |-> p.seq]}
 /\ messages' = messages \cup {[zone |-> z, from |-> p.from, to |-> p.to, seq |-> p.seq]}
 /\ UNCHANGED <<active, crashed, backups, turn>>
Install(z) ==
 LET p == pending[z] IN
 /\ p.phase = "decided" /\ p.to \notin crashed
 /\ p.seq > consumed[p.to][z]
 /\ installed' = [installed EXCEPT ![p.to][z] = p.seq]
 /\ active' = [active EXCEPT ![z] = [holder |-> p.to, seq |-> p.seq]]
 /\ pending' = [pending EXCEPT ![z].phase = "installed"]
 /\ UNCHANGED <<consumed, history, messages, crashed, backups, turn>>
Finish(z) ==
 /\ pending[z].phase = "installed"
 /\ pending' = [pending EXCEPT ![z].phase = "idle"]
 /\ UNCHANGED <<active, installed, consumed, history, messages, crashed, backups, turn>>
Compact ==
 /\ history # {}
 /\ history' = {}
 /\ UNCHANGED <<active, installed, consumed, pending, messages, crashed, backups, turn>>
Replay(m) ==
 LET n == m.to z == m.zone s == m.seq IN
 /\ n \notin crashed
 /\ IF UnsafeReplay THEN history = {} ELSE s > consumed[n][z]
 /\ installed' = [installed EXCEPT ![n][z] = s]
 /\ UNCHANGED <<active, consumed, pending, history, messages, crashed, backups, turn>>
Crash(n) ==
 /\ n \notin crashed /\ turn < 1
 /\ crashed' = crashed \cup {n}
 /\ turn' = turn + 1
 /\ UNCHANGED <<active, installed, consumed, pending, history, messages, backups>>
Recover(n) ==
 /\ n \in crashed
 /\ crashed' = crashed \ {n}
 /\ UNCHANGED <<active, installed, consumed, pending, history, messages, backups, turn>>
Restore(n) ==
 /\ n \notin crashed /\ turn < 2
 /\ installed' = [installed EXCEPT ![n] = [z \in Zones |-> IF UnsafeRestore \/ backups[n][z] > consumed[n][z] THEN backups[n][z] ELSE -1]]
 /\ consumed' = IF UnsafeRestore THEN [consumed EXCEPT ![n] = [z \in Zones |-> -1]] ELSE consumed
 /\ turn' = turn + 1
 /\ UNCHANGED <<active, pending, history, messages, crashed, backups>>
Next == \/ \E z \in Zones, n \in Nodes : Begin(z,n)
        \/ \E z \in Zones : Fence(z) \/ Install(z) \/ Finish(z)
        \/ Compact
        \/ \E m \in messages : Replay(m)
        \/ \E n \in Nodes : Crash(n) \/ Recover(n) \/ Restore(n)
SingleWriter == \A z \in Zones : Cardinality({n \in Nodes : installed[n][z] >= 0 /\ n \notin crashed}) <= 1
NoConsumedWriter == \A n \in Nodes,z \in Zones : installed[n][z] >= 0 => installed[n][z] > consumed[n][z]
Spec == Init /\ [][Next]_vars
=============================================================================
