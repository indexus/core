------------------------- MODULE CoalescedRights -------------------------
EXTENDS Naturals, FiniteSets
CONSTANT UnsafeMerge
Tokens == {"left", "right", "parent", "delivered"}
Coverage(t) == IF t="left" THEN {0} ELSE IF t="right" THEN {1} ELSE {0,1}
VARIABLES atA, atB, consumed, pending, history, issued
vars == <<atA,atB,consumed,pending,history,issued>>
Init == /\ atA={"left","right"} /\ atB={} /\ consumed={} /\ pending=FALSE /\ history={} /\ issued={"left","right"}
Merge == /\ {"left","right"} \subseteq atA
         /\ atA'=(atA \ (IF UnsafeMerge THEN {"left"} ELSE {"left","right"})) \cup {"parent"}
         /\ consumed'=consumed \cup (IF UnsafeMerge THEN {"left"} ELSE {"left","right"})
         /\ history'=history \cup {"merged"}
         /\ issued'=issued \cup {"parent"}
         /\ UNCHANGED <<atB,pending>>
Fence == /\ "parent" \in atA /\ ~pending
         /\ atA'=atA \ {"parent"} /\ consumed'=consumed \cup {"parent"}
         /\ pending'=TRUE /\ history'=history \cup {"sent"}
         /\ UNCHANGED <<atB,issued>>
Install == /\ pending /\ atB'={"delivered"} /\ pending'=FALSE
           /\ UNCHANGED <<atA,consumed,history,issued>>
Compact == /\ history # {} /\ history'={} /\ UNCHANGED <<atA,atB,consumed,pending,issued>>
Replay(t) == /\ t \in issued /\ t \in {"left","right","parent"} /\ t \notin consumed
             /\ atA'=atA \cup {t} /\ UNCHANGED <<atB,consumed,pending,history,issued>>
Next == Merge \/ Fence \/ Install \/ Compact \/ \E t \in Tokens : Replay(t)
SingleWriter == \A z \in {0,1} : ~( (\E t \in atA : z \in Coverage(t)) /\ (\E t \in atB : z \in Coverage(t)) )
Spec == Init /\ [][Next]_vars
=============================================================================
