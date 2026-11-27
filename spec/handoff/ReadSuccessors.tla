------------------------- MODULE ReadSuccessors -------------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS RecursiveReads, PreserveDirectory, ClearIncoming
Nodes == {"A", "B", "C"}
None == "none"
Target == <<"B", "C", "A", "B">>
VARIABLES owner, successor, receipts, turn, started, pending, visited, seen, done
vars == <<owner, successor, receipts, turn, started, pending, visited, seen, done>>
Init == /\ owner = "A" /\ successor = [n \in Nodes |-> None]
        /\ receipts = {} /\ turn = 0 /\ started = FALSE
        /\ pending = {} /\ visited = {} /\ seen = FALSE /\ done = FALSE
\* Each Move abstracts a completed pairwise handoff with its two durable
\* commits. Crash cuts/point installs are checked by the Go transaction harness.
Move == /\ ~started /\ turn < 4
        /\ owner' = Target[turn + 1]
        /\ successor' = [n \in Nodes |->
              IF n = owner THEN owner'
              ELSE IF n = owner' /\ ClearIncoming THEN None ELSE successor[n]]
        /\ receipts' = receipts \cup {owner} /\ turn' = turn + 1
        /\ UNCHANGED <<started, pending, visited, seen, done>>
Compact(n) == /\ n \in receipts /\ receipts' = receipts \ {n}
              /\ successor' = IF PreserveDirectory THEN successor
                               ELSE [successor EXCEPT ![n] = None]
              /\ UNCHANGED <<owner, turn, started, pending, visited, seen, done>>
Begin == /\ turn > 0 /\ ~started /\ started' = TRUE /\ pending' = {"A"}
         /\ UNCHANGED <<owner, successor, receipts, turn, visited, seen, done>>
Read(n) == /\ started /\ ~done /\ n \in pending
           /\ visited' = visited \cup {n}
           /\ seen' = (seen \/ (n = owner))
           /\ pending' = (pending \ {n}) \cup
                (IF successor[n] # None /\ successor[n] \notin visited'
                    /\ (RecursiveReads \/ visited = {})
                 THEN {successor[n]} ELSE {})
           /\ UNCHANGED <<owner, successor, receipts, turn, started, done>>
Finish == /\ started /\ pending = {} /\ ~done /\ done' = TRUE
          /\ UNCHANGED <<owner, successor, receipts, turn, started, pending, visited, seen>>
Next == Move \/ Begin \/ Finish \/ (\E n \in Nodes: Compact(n) \/ Read(n))
        \/ (done /\ UNCHANGED vars)
CompleteRead == done => seen
NoCycles == /\ \A n \in Nodes: successor[n] # n
            /\ ~\E a,b \in Nodes: successor[a] = b /\ successor[b] = a
            /\ ~\E a,b,c \in Nodes: successor[a] = b /\ successor[b] = c /\ successor[c] = a
EventuallyRead == <>done
Spec == Init /\ [][Next]_vars /\ WF_vars(Move) /\ WF_vars(Begin)
        /\ WF_vars(Finish) /\ (\A n \in Nodes: WF_vars(Read(n)))
=============================================================================
