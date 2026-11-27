------------------------------ MODULE TicketData ------------------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS CopyPayload, RequireInstallACK, ProtectCurrentData
Nodes == {"A", "B"}
MaxTransfer == 2
Transfers == 1..MaxTransfer
None == MaxTransfer+1
Source(e) == IF e % 2 = 1 THEN "A" ELSE "B"
Target(e) == IF e % 2 = 1 THEN "B" ELSE "A"
VARIABLES native, counter, open, data, latest, payload, issued, installed, receipts,
          acked, done, online, faults
vars == <<native, counter, open, data, latest, payload, issued, installed, receipts,
          acked, done, online, faults>>
Init == /\ native = [n \in Nodes |-> IF n = "A" THEN 0 ELSE None]
        /\ counter = [n \in Nodes |-> 0] /\ open = {} /\ data = [n \in Nodes |-> 0] /\ latest = 0
        /\ payload = [e \in Transfers |-> 0]
        /\ issued = {} /\ installed = {} /\ receipts = {} /\ acked = {} /\ done = {}
        /\ online = Nodes /\ faults = TRUE
Owns(n) == native[n] # None
Write(n) == /\ n \in online /\ Owns(n) /\ latest = 0
            /\ latest' = 1 /\ data' = [data EXCEPT ![n] = 1]
            /\ UNCHANGED <<native, counter, open, payload, issued, installed, receipts, acked, done, online, faults>>
Emit(n) == LET e == native[n]+1
           IN /\ n \in online /\ Owns(n) /\ e <= MaxTransfer
              /\ native' = [native EXCEPT ![n] = None]
              /\ counter' = [counter EXCEPT ![Target(e)] = e]
              /\ open' = open \cup {e}
              /\ issued' = issued \cup {e} /\ payload' = [payload EXCEPT ![e] = data[n]]
              /\ UNCHANGED <<data, latest, installed, receipts, acked, done, online, faults>>
Install(e) == /\ e \in issued /\ Target(e) \in online
              /\ native[Target(e)] = None /\ e \in open
              /\ native' = [native EXCEPT ![Target(e)] = e]
              /\ open' = open \ {e}
              /\ UNCHANGED counter
              /\ data' = [data EXCEPT ![Target(e)] = IF CopyPayload THEN payload[e] ELSE 0]
              /\ installed' = installed \cup {e} /\ receipts' = receipts \cup {e}
              /\ UNCHANGED <<latest, payload, issued, acked, done, online, faults>>
Reply(e) == /\ e \in issued \ acked /\ Target(e) \in online
            /\ (e <= counter[Target(e)] /\ e \notin open)
            /\ acked' = acked \cup {e}
            /\ UNCHANGED <<native, counter, open, data, latest, payload, issued, installed, receipts, done, online, faults>>
Complete(e) == /\ e \in issued \ done /\ Source(e) \in online
               /\ (~RequireInstallACK \/ e \in acked)
               /\ done' = done \cup {e}
               /\ data' = [data EXCEPT ![Source(e)] =
                     IF ProtectCurrentData /\ Owns(Source(e)) THEN @ ELSE 0]
               /\ UNCHANGED <<native, counter, open, latest, payload, issued, installed, receipts, acked, online, faults>>
Compact(e) == /\ e \in receipts /\ Target(e) \in online /\ receipts' = receipts \ {e}
              /\ UNCHANGED <<native, counter, open, data, latest, payload, issued, installed, acked, done, online, faults>>
Crash(n) == /\ faults /\ n \in online /\ online' = online \ {n}
            /\ UNCHANGED <<native, counter, open, data, latest, payload, issued, installed, receipts, acked, done, faults>>
Recover(n) == /\ n \in Nodes \ online /\ online' = online \cup {n}
              /\ UNCHANGED <<native, counter, open, data, latest, payload, issued, installed, receipts, acked, done, faults>>
StopFaults == /\ faults /\ faults' = FALSE
              /\ UNCHANGED <<native, counter, open, data, latest, payload, issued, installed, receipts, acked, done, online>>
Next == (\E n \in Nodes: Write(n) \/ Emit(n) \/ Crash(n) \/ Recover(n))
        \/ (\E e \in Transfers: Install(e) \/ Reply(e) \/ Complete(e) \/ Compact(e))
        \/ StopFaults \/ (issued \subseteq done /\ UNCHANGED vars)
SingleWriter == Cardinality({n \in Nodes: Owns(n)}) <= 1
WriterHasLatest == \A n \in Nodes: Owns(n) => data[n] = latest
ACKRetained == (\E n \in Nodes: data[n] = latest)
                \/ (\E e \in issued \ done: payload[e] = latest)
SoundObsoleteACK == acked \subseteq installed
EveryTransferCloses == \A e \in Transfers: (e \in issued) ~> (e \in done)
Spec == Init /\ [][Next]_vars /\ WF_vars(StopFaults)
        /\ (\A n \in Nodes: WF_vars(Recover(n)))
        /\ (\A e \in Transfers: WF_vars(Install(e)) /\ WF_vars(Reply(e)) /\ WF_vars(Complete(e)))
=============================================================================
