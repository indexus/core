------------------------- MODULE ReceiverTickets -------------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS ReopenClosed, PreserveOpen, FenceSource
Nodes == {"A", "B"}
Cells == {0, 1}
MaxSeq == 3
Ops == Nodes \X (1..MaxSeq)
Slots == Nodes \X (1..MaxSeq)
Tokens == Nodes \X (0..MaxSeq) \X {0, 1}
Output(o,i) == <<o[1],o[2],i>>
Grant(n,scope) == [holder |-> n, scope |-> scope]
VARIABLES rights, grants, counters, offers, parent, ticket, active, bound,
          open, slotOp, confirmed, ready, issued, installed, aborted,
          receipts, acked, done, online, faults
vars == <<rights, grants, counters, offers, parent, ticket, active, bound,
          open, slotOp, confirmed, ready, issued, installed, aborted,
          receipts, acked, done, online, faults>>
Init == /\ rights = {<<"A",0,0>>, <<"A",0,1>>}
        /\ grants = [t \in Tokens |-> IF t = <<"A",0,0>> THEN Grant("A",{0})
                    ELSE IF t = <<"A",0,1>> THEN Grant("A",{1}) ELSE Grant("A",{})]
        /\ counters = [n \in Nodes |-> 0] /\ offers = {} /\ active = {}
        /\ parent = [o \in Ops |-> <<"A",0,0>>] /\ ticket = [o \in Ops |-> 0]
        /\ slotOp = [s \in Slots |-> <<"A",1>>]
        /\ bound = {} /\ open = {} /\ confirmed = {} /\ ready = {}
        /\ issued = {} /\ installed = {} /\ aborted = {} /\ receipts = {}
        /\ acked = {} /\ done = {} /\ online = Nodes /\ faults = TRUE
Destination(o) == grants[Output(o,0)].holder
Slot(o) == <<Destination(o),ticket[o]>>
Matching(o) == IF ticket[o] = 0 THEN FALSE
              ELSE Slot(o) \in open /\ slotOp[Slot(o)] = o
Closed(o) == ticket[o] > 0 /\ ticket[o] <= counters[Destination(o)] /\ ~Matching(o)
Owns(n,c) == \E t \in rights: grants[t].holder = n /\ c \in grants[t].scope
Reserve(t,target,piece) ==
    LET g == grants[t]
        n == g.holder
        o == <<n,counters[n]+1>>
    IN /\ t \in rights /\ n \in online /\ counters[n] < MaxSeq
       /\ target \in Nodes \ {n} /\ piece \in (SUBSET g.scope) \ {{}}
       /\ ~\E p \in active \ issued: parent[p] = t
       /\ counters' = [counters EXCEPT ![n] = @+1]
       /\ offers' = offers \cup {o} /\ active' = active \cup {o}
       /\ parent' = [parent EXCEPT ![o] = t]
       /\ grants' = [grants EXCEPT ![Output(o,0)] = Grant(target,piece),
                                   ![Output(o,1)] = Grant(n,g.scope \ piece)]
       /\ UNCHANGED <<rights,ticket,bound,open,slotOp,confirmed,ready,issued,
                      installed,aborted,receipts,acked,done,online,faults>>
Admit(o) ==
    LET n == Destination(o)
        s == <<n,counters[n]+1>>
    IN /\ o \in offers /\ n \in online /\ counters[n] < MaxSeq
       /\ ~\E old \in open: slotOp[old] = o
       /\ counters' = [counters EXCEPT ![n] = @+1]
       /\ open' = open \cup {s} /\ slotOp' = [slotOp EXCEPT ![s] = o]
       /\ UNCHANGED <<rights,grants,offers,parent,ticket,active,bound,confirmed,
                      ready,issued,installed,aborted,receipts,acked,done,online,faults>>
Bind(o,s) == /\ o \in active /\ o[1] \in online /\ ticket[o] = 0
             /\ s \in open /\ slotOp[s] = o
             /\ ticket' = [ticket EXCEPT ![o] = s[2]] /\ bound' = bound \cup {o}
             /\ UNCHANGED <<rights,grants,counters,offers,parent,active,open,slotOp,
                            confirmed,ready,issued,installed,aborted,receipts,acked,done,online,faults>>
Confirm(s) == /\ s \in open \ confirmed /\ s[1] \in online
              /\ slotOp[s] \in bound /\ Slot(slotOp[s]) = s
              /\ confirmed' = confirmed \cup {s}
              /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,
                             slotOp,ready,issued,installed,aborted,receipts,acked,done,online,faults>>
Ready(o) == /\ o \in active \ ready /\ o[1] \in online /\ Matching(o)
            /\ Slot(o) \in confirmed /\ ready' = ready \cup {o}
            /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,
                           slotOp,confirmed,issued,installed,aborted,receipts,acked,done,online,faults>>
Emit(o) ==
    LET rest == grants[Output(o,1)].scope
    IN /\ o \in (active \ issued) \cap ready /\ o[1] \in online /\ parent[o] \in rights
       /\ rights' = IF FenceSource THEN (rights \ {parent[o]}) \cup
                      (IF rest = {} THEN {} ELSE {Output(o,1)}) ELSE rights
       /\ issued' = issued \cup {o}
       /\ UNCHANGED <<grants,counters,offers,parent,ticket,active,bound,open,slotOp,
                      confirmed,ready,installed,aborted,receipts,acked,done,online,faults>>
Install(o) ==
    LET g == grants[Output(o,0)]
    IN /\ o \in issued /\ g.holder \in online /\ Output(o,0) \notin rights
       /\ ((Matching(o) /\ Slot(o) \in confirmed) \/ (ReopenClosed /\ Closed(o)))
       /\ ~\E t \in rights: grants[t].holder = g.holder /\ grants[t].scope \cap g.scope # {}
       /\ rights' = rights \cup {Output(o,0)}
       /\ open' = open \ {Slot(o)} /\ confirmed' = confirmed \ {Slot(o)}
       /\ installed' = installed \cup {o} /\ receipts' = receipts \cup {o}
       /\ UNCHANGED <<grants,counters,offers,parent,ticket,active,bound,slotOp,
                      ready,issued,aborted,acked,done,online,faults>>
Reply(o) == /\ o \in issued \ acked /\ Destination(o) \in online /\ Closed(o)
            /\ acked' = acked \cup {o}
            /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,
                           slotOp,confirmed,ready,issued,installed,aborted,receipts,done,online,faults>>
Complete(o) == /\ o \in acked \ done /\ o[1] \in online
               /\ done' = done \cup {o} /\ active' = active \ {o}
               /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,bound,open,slotOp,
                              confirmed,ready,issued,installed,aborted,receipts,acked,online,faults>>
Abort(o) == /\ o \in active \ issued /\ o[1] \in online
            /\ active' = active \ {o} /\ aborted' = aborted \cup {o}
            /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,bound,open,slotOp,
                           confirmed,ready,issued,installed,receipts,acked,done,online,faults>>
Cancel(s) == /\ s \in open /\ s[1] \in online /\ slotOp[s] \in aborted
             /\ open' = open \ {s} /\ confirmed' = confirmed \ {s}
             /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,slotOp,
                            ready,issued,installed,aborted,receipts,acked,done,online,faults>>
Expire(s) == /\ s \in open \ confirmed /\ s[1] \in online /\ open' = open \ {s}
             /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,slotOp,
                            confirmed,ready,issued,installed,aborted,receipts,acked,done,online,faults>>
Forget(s) == /\ ~PreserveOpen /\ s \in open \cap confirmed /\ s[1] \in online
             /\ open' = open \ {s} /\ confirmed' = confirmed \ {s}
             /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,slotOp,
                            ready,issued,installed,aborted,receipts,acked,done,online,faults>>
Compact(o) == /\ o \in receipts /\ Destination(o) \in online /\ receipts' = receipts \ {o}
              /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,slotOp,
                             confirmed,ready,issued,installed,aborted,acked,done,online,faults>>
Coalesce(a,b) ==
    LET x == grants[a]
        y == grants[b]
        n == x.holder
        merged == <<n,counters[n]+1,0>>
    IN /\ a \in rights /\ b \in rights /\ a # b /\ n = y.holder /\ n \in online
       /\ x.scope \cap y.scope = {} /\ x.scope \cup y.scope = Cells /\ counters[n] < MaxSeq
       /\ rights' = (rights \ {a,b}) \cup {merged}
       /\ grants' = [grants EXCEPT ![merged] = Grant(n,Cells)]
       /\ counters' = [counters EXCEPT ![n] = @+1]
       /\ UNCHANGED <<offers,parent,ticket,active,bound,open,slotOp,confirmed,ready,
                      issued,installed,aborted,receipts,acked,done,online,faults>>
Crash(n) == /\ faults /\ n \in online /\ online' = online \ {n}
            /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,slotOp,
                           confirmed,ready,issued,installed,aborted,receipts,acked,done,faults>>
Recover(n) == /\ n \in Nodes \ online /\ online' = online \cup {n}
              /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,slotOp,
                             confirmed,ready,issued,installed,aborted,receipts,acked,done,faults>>
StopFaults == /\ faults /\ faults' = FALSE
              /\ UNCHANGED <<rights,grants,counters,offers,parent,ticket,active,bound,open,slotOp,
                             confirmed,ready,issued,installed,aborted,receipts,acked,done,online>>
Next == (\E t \in Tokens,n \in Nodes,p \in (SUBSET Cells): Reserve(t,n,p))
        \/ (\E o \in Ops: Admit(o) \/ Ready(o) \/ Emit(o) \/ Install(o) \/ Reply(o) \/ Complete(o) \/ Abort(o) \/ Compact(o))
        \/ (\E o \in Ops,s \in Slots: Bind(o,s))
        \/ (\E s \in Slots: Confirm(s) \/ Cancel(s) \/ Expire(s) \/ Forget(s))
        \/ (\E a,b \in Tokens: Coalesce(a,b))
        \/ (\E n \in Nodes: Crash(n) \/ Recover(n)) \/ StopFaults
        \/ (issued \subseteq done /\ UNCHANGED vars)
TwoOffers == Cardinality(offers) <= 2
SingleWriter == \A c \in Cells: Cardinality({n \in Nodes: Owns(n,c)}) <= 1
SoundACK == acked \subseteq installed
ConfirmedUntilInstall == \A o \in issued \ installed: Matching(o) /\ Slot(o) \in confirmed
EveryIssuedCloses == \A o \in Ops: (o \in issued) ~> (o \in done)
Spec == Init /\ [][Next]_vars /\ WF_vars(StopFaults)
        /\ (\A n \in Nodes: WF_vars(Recover(n)))
        /\ (\A o \in Ops: WF_vars(Install(o)) /\ WF_vars(Reply(o)) /\ WF_vars(Complete(o)))
=============================================================================
