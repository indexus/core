------------------------- MODULE GranularRecovery -------------------------
EXTENDS Naturals, TLC
CONSTANTS Zones, Parent, Children, MaxSeq, RetryNeedsMetadata, UnsafeThaw
VARIABLES phase, fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed
vars == <<phase, fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed>>
(* Durable phases: 0=warm, 1=copied, 2=frozen, 3=certified, 4=emitted,
   5=installed, 6=installation ACK. Global metadata builder stays unavailable
   after initial preparation, while the durable dependencies remain held. *)
Init == /\ phase = [z \in Zones |-> 0] /\ fence = [z \in Zones |-> FALSE]
        /\ sourceSeq = [z \in Zones |-> 0] /\ receiverSeq = [z \in Zones |-> 0]
        /\ ackSeq = [z \in Zones |-> 0] /\ ready = TRUE
        /\ faultLeft = TRUE /\ connected = TRUE /\ closed = FALSE
SourceWrite(z) == /\ ~fence[z] /\ sourceSeq[z] < MaxSeq
 /\ sourceSeq' = [sourceSeq EXCEPT ![z] = @ + 1]
 /\ ackSeq' = [ackSeq EXCEPT ![z] = sourceSeq'[z]]
 /\ UNCHANGED <<phase, fence, receiverSeq, ready, faultLeft, connected, closed>>
ReceiverWrite(z) == /\ phase[z] >= 5 /\ receiverSeq[z] < MaxSeq
 /\ receiverSeq' = [receiverSeq EXCEPT ![z] = @ + 1]
 /\ ackSeq' = [ackSeq EXCEPT ![z] = receiverSeq'[z]]
 /\ UNCHANGED <<phase, fence, sourceSeq, ready, faultLeft, connected, closed>>
Copy(z) == /\ phase[z] = 0 /\ connected /\ ready
 /\ phase' = [phase EXCEPT ![z] = 1]
 /\ receiverSeq' = [receiverSeq EXCEPT ![z] = sourceSeq[z]]
 /\ UNCHANGED <<fence, sourceSeq, ackSeq, ready, faultLeft, connected, closed>>
Freeze(z) == /\ phase[z] = 1 /\ ready
 /\ (z = Parent => \A c \in Children : phase[c] >= 4)
 /\ phase' = [phase EXCEPT ![z] = 2] /\ fence' = [fence EXCEPT ![z] = TRUE]
 /\ UNCHANGED <<sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed>>
Stage(z) == /\ phase[z] = 2 /\ connected /\ ready
 /\ phase' = [phase EXCEPT ![z] = 3]
 (* Final mirror and durable seal certification abstracted into one step. *)
 /\ receiverSeq' = [receiverSeq EXCEPT ![z] = sourceSeq[z]]
 /\ UNCHANGED <<fence, sourceSeq, ackSeq, ready, faultLeft, connected, closed>>
Emit(z) == /\ phase[z] = 3 /\ ready
 /\ phase' = [phase EXCEPT ![z] = 4]
 /\ UNCHANGED <<fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed>>
Install(z) == /\ phase[z] = 4 /\ connected /\ ready
 /\ phase' = [phase EXCEPT ![z] = 5]
 /\ UNCHANGED <<fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed>>
Ack(z) == /\ phase[z] = 5 /\ connected /\ ready
 /\ phase' = [phase EXCEPT ![z] = 6]
 /\ UNCHANGED <<fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed>>
(* Lost response/crash preserves durable phases, bytes and fences. *)
Fault == /\ faultLeft /\ \E z \in Zones : phase[z] >= 2 /\ phase[z] <= 5
 /\ connected' = FALSE /\ ready' = FALSE /\ faultLeft' = FALSE
 /\ UNCHANGED <<phase, fence, sourceSeq, receiverSeq, ackSeq, closed>>
Reconnect == /\ ~connected /\ connected' = TRUE
 /\ UNCHANGED <<phase, fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, closed>>
Retry == /\ connected /\ ~ready /\ ~RetryNeedsMetadata /\ ready' = TRUE
 /\ UNCHANGED <<phase, fence, sourceSeq, receiverSeq, ackSeq, faultLeft, connected, closed>>
(* Deliberately incorrect mutation: a timeout must not reopen emitted authority. *)
Thaw(z) == /\ UnsafeThaw /\ phase[z] >= 4 /\ fence[z]
 /\ fence' = [fence EXCEPT ![z] = FALSE]
 /\ UNCHANGED <<phase, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected, closed>>
Close == /\ ~closed /\ \A z \in Zones : phase[z] = 6
 /\ closed' = TRUE
 /\ UNCHANGED <<phase, fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft, connected>>
Next == (\E z \in Zones : SourceWrite(z) \/ ReceiverWrite(z) \/ Copy(z) \/ Freeze(z)
                         \/ Stage(z) \/ Emit(z) \/ Install(z) \/ Ack(z) \/ Thaw(z))
        \/ Fault \/ Reconnect \/ Retry \/ Close
Spec == Init /\ [][Next]_vars /\ WF_vars(Reconnect) /\ WF_vars(Retry) /\ WF_vars(Close)
 /\ \A z \in Zones : WF_vars(Copy(z)) /\ WF_vars(Freeze(z)) /\ WF_vars(Stage(z))
                    /\ WF_vars(Emit(z)) /\ WF_vars(Install(z)) /\ WF_vars(Ack(z))
TypeOK == /\ phase \in [Zones -> 0..6] /\ fence \in [Zones -> BOOLEAN]
 /\ sourceSeq \in [Zones -> 0..MaxSeq] /\ receiverSeq \in [Zones -> 0..MaxSeq]
 /\ ackSeq \in [Zones -> 0..MaxSeq]
SingleWriter == \A z \in Zones : ~(~fence[z] /\ phase[z] >= 5)
ACKReadable == \A z \in Zones : IF phase[z] >= 5 THEN receiverSeq[z] >= ackSeq[z]
                              ELSE sourceSeq[z] >= ackSeq[z]
ParentAfterChildren == phase[Parent] >= 2 => \A c \in Children : phase[c] >= 4
ClosedAfterACKs == closed => \A z \in Zones : phase[z] = 6
EventuallyClosed == <>closed
=============================================================================
